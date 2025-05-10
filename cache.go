package pgproxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/redis/go-redis/v9"
)

var (
	// Whether we'll enable `Bind` command caching
	PGPROXY_ENABLE_CACHE bool

	// Whether we'll check the `Bind` message parameters for a hook (custom behaviour for the proxy)
	PGPROXY_ENABLE_PARAMETER_HOOK bool

	// Which parameter position we'll check for the hook value. Valid values are `first` or `last`
	PGPROXY_PARAMETER_HOOK_POSITION string = "first"

	// How long to cache the postgresql command
	PGPROXY_CACHE_TTL_SECOND int64 = 3600

	// Cache jitter
	PGPROXY_CACHE_TTL_JITTER_SECOND int64 = 5
)

type CacheI interface {
	// Returns list of postgres commands to be sent back to the frontend
	Get(key string) (*Cache, bool)

	// Saves postgres commands as a cache of the given key
	Set(key string, val Cache) error

	// Delete `key` as a pattern, will be prepended with `PGPROXY` cache key
	// so we don't accidentally conflict with other keys
	DelPattern(key string)
}

// Buffered postgresql data
//
// We buffer the cached data due to the asynchronous nature of the postgres request/response
// message flow -- a single query request will have a bunch of responses returned by the backend, at least 1 for each
// records, along with other "metadata" messages
//
// The expected running state of this buffer is:
//
// 1. On start: any frontend request will be ignored
// 2. On getting a frontend `Bind` request, we'll start the cache process
// 3. We'll then listen to all of backend `DataRow` message until a `CommandComplete` is reached
// 4. The cache buffer is completed & we'll cache the buffered data
// 5. Go to 1
//
// The cache checks itself is done by checking the existing key on step 2, and if found then we'll immediately send
// `BindComplete`, `DataRow`, and `CommandComplete` messages based on the cached data. The `Bind` frontend request will not
// be sent to the backend in this case
type CacheBuffer struct {
	*sync.Mutex
	Client CacheI

	frontendQuery bool   // Whether frontend requests the query or not
	hook          hook   // Statement parameter hook
	key           string // Cache key, hashed from frontend request & used on backend result

	// Buffered postgres commands: Frontend needs to hold both frontend commands, for when
	// there's a failure & we need to keep forwarding the command, and
	// backend commands for the actual cached data
	frontend buff

	// Buffered postgres commands: backend only needs to buffer backend command since we
	// need to whole data to be available first before it can be cached
	backend bufb
}

func NewCacheBuffer(client CacheI) *CacheBuffer {
	return &CacheBuffer{Mutex: new(sync.Mutex), Client: client}
}

// Return value indicate whether the proxy should forward the frontend message to the backend or not
func (c *CacheBuffer) CacheFrontend(
	addr string,
	msg pgproto3.FrontendMessage,
	frontend *pgproto3.Frontend,
	backend *pgproto3.Backend,
	errChan chan<- error,
) bool {
	switch msg := msg.(type) {
	case *pgproto3.Query:
		return c.cacheFrontendQuery(msg, errChan)
	case *pgproto3.Bind:
		return c.cacheFrontendBind(msg, errChan)
	case *pgproto3.Describe:
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if c.frontend.cached == nil {
			c.resetf()
			return true
		}
		c.frontend.describe = &pgproto3.Describe{
			ObjectType: msg.ObjectType,
			Name:       msg.Name,
		}
		return false
	case *pgproto3.Execute:
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if c.frontend.cached == nil {
			c.resetf()
			return true
		}
		c.frontend.execute = &pgproto3.Execute{
			Portal:  msg.Portal,
			MaxRows: msg.MaxRows,
		}
		return false
	case *pgproto3.Sync:
		c.Mutex.Lock()
		if c.frontend.cached == nil {
			c.resetf()
			c.Mutex.Unlock()
			return true
		}
		key, buf := key(c.key, c.hook.keySuffix), c.frontend
		c.resetf()
		c.Mutex.Unlock()
		if !sendCache(
			addr,
			backend,
			buf.describe,
			buf.cached.rowDescription,
			buf.cached.dataRow,
			buf.cached.commandComplete,
			buf.cached.readyForQuery,
			errChan,
		) {
			// Cache data issue, we'll release the buffered messages to the backend & let the query run as normal
			slog.Info("pgproxy_cache_hit_but_invalid", "key", key)
			if buf.query != nil {
				frontend.Send(buf.query)
				if err := frontend.Flush(); err != nil {
					terminate(errChan, errors.Join(errors.New("error when sending message to backend (`Query` command invalid cache)"), err))
					return false
				}
			} else if buf.bind != nil {
				frontend.Send(buf.bind)
				if err := frontend.Flush(); err != nil {
					terminate(errChan, errors.Join(errors.New("error when sending message to backend (`Bind` command invalid cache)"), err))
					return false
				}
			}
			if buf.describe != nil {
				frontend.Send(buf.describe)
				if err := frontend.Flush(); err != nil {
					terminate(errChan, errors.Join(errors.New("error when sending message to backend (`Describe` command invalid cache)"), err))
					return false
				}
			}
			frontend.Send(buf.execute)
			if err := frontend.Flush(); err != nil {
				terminate(errChan, errors.Join(errors.New("error when sending message to backend (`Execute` command invalid cache)"), err))
				return false
			}
			return true
		}
		return false
	default:
		return true
	}
}

func (c *CacheBuffer) cacheFrontendBind(msg *pgproto3.Bind, errChan chan<- error) bool {
	hook := newHookFromBind(msg)

	// Invalidating cache key here might be a bit hacky: deleting the key *immediately* before the mutating query
	// is executed means that another cache might get set which still have the stale data on the database.
	//
	// It *might* be okay to just delete it some time after few seconds has passed, if the mutating query is longer
	// than that then the cached data not getting updated would probably be expected as usual
	go func(keys []string) {
		time.Sleep(3 * time.Second)
		invalidate(c.Client, keys)
	}(slices.Clone(hook.invalidate))

	if hook.noCache {
		return true
	}

	// The whole command is used as the cache key with the exception of the hook parameter, if it is set
	clearHookParameter(msg)
	encoded, err := msg.Encode(nil)
	if err != nil {
		terminate(errChan, errors.Join(errors.New("invalid frontend message"), err))
		return false
	}
	cachekey, msgcopy := hash(encoded), &pgproto3.Bind{
		DestinationPortal:    msg.DestinationPortal,
		PreparedStatement:    msg.PreparedStatement,
		ParameterFormatCodes: make([]int16, len(msg.ParameterFormatCodes)),
		Parameters: func() [][]byte {
			parameters := make([][]byte, len(msg.Parameters))
			for i := range msg.Parameters {
				if msg.Parameters[i] == nil {
					parameters[i] = nil
				} else {
					parameters[i] = make([]byte, len(msg.Parameters[i]))
					copy(parameters[i], msg.Parameters[i])
				}
			}
			return parameters
		}(),
		ResultFormatCodes: make([]int16, len(msg.ResultFormatCodes)),
	}
	copy(msgcopy.ParameterFormatCodes, msg.ParameterFormatCodes)
	copy(msgcopy.ResultFormatCodes, msg.ResultFormatCodes)

	c.Mutex.Lock()
	c.frontendQuery = true
	c.hook = hook
	c.key = cachekey
	c.frontend.bind = msgcopy
	c.Mutex.Unlock()

	// Cache key needs to be checked here since if there's any cache hit, we need to stop sending any
	// request further to the backend, since otherwise the request sent to the backend might be "incorrect" in that
	// some request states would get skipped
	if msgs, ok := c.Client.Get(key(cachekey, hook.keySuffix)); ok {
		rowDescription, datarows, command, ready := msgs.ToCommand()
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if c.frontend.cached != nil {
			terminate(errChan, errors.New("invalid 'Bind' command sent from frontend, already bound"))
			return false
		}
		c.frontend.cached = &bufb{rowDescription, datarows, command, ready}
		slog.Info("pgproxy_cache_hit", "key", key(c.key, c.hook.keySuffix))
		return false
	}

	// On any cache error, just forward the message to backend & skip any cache
	return true
}

func (c *CacheBuffer) cacheFrontendQuery(msg *pgproto3.Query, errChan chan<- error) bool {
	hook := newHookFromQuery(msg)
	encoded, err := msg.Encode(nil)
	if err != nil {
		terminate(errChan, errors.Join(errors.New("invalid frontend message"), err))
		return false
	}
	cachekey, msgcopy := hash(encoded), &pgproto3.Query{
		String: msg.String,
	}

	c.Mutex.Lock()
	c.frontendQuery = true
	c.hook = hook
	c.key = cachekey
	c.frontend.query = msgcopy
	c.Mutex.Unlock()

	// Cache key needs to be checked here since if there's any cache hit, we need to stop sending any
	// request further to the backend, since otherwise the request sent to the backend might be "incorrect" in that
	// some request states would get skipped
	if msgs, ok := c.Client.Get(key(cachekey, hook.keySuffix)); ok {
		rowDescription, datarows, command, ready := msgs.ToCommand()
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if c.frontend.cached != nil {
			terminate(errChan, errors.New("invalid 'Query' command sent from frontend, already bound"))
			return false
		}
		c.frontend.cached = &bufb{rowDescription, datarows, command, ready}
		slog.Info("pgproxy_cache_hit", "key", key(c.key, c.hook.keySuffix))
		return false
	}

	// On any cache error, just forward the message to backend & skip any cache
	return true
}

func (c *CacheBuffer) CacheBackend(msg pgproto3.BackendMessage, errChan chan<- error) {
	switch msg := msg.(type) {
	case *pgproto3.RowDescription:
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if !c.frontendQuery {
			c.resetb()
			return
		}
		c.backend.rowDescription = &pgproto3.RowDescription{
			Fields: make([]pgproto3.FieldDescription, len(msg.Fields)),
		}
		copy(c.backend.rowDescription.Fields, msg.Fields)
		for i := range c.backend.rowDescription.Fields {
			if c.backend.rowDescription.Fields[i].Name != nil {
				c.backend.rowDescription.Fields[i].Name = make([]byte, len(msg.Fields[i].Name))
				copy(c.backend.rowDescription.Fields[i].Name, msg.Fields[i].Name)
			}
		}
	case *pgproto3.DataRow:
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if !c.frontendQuery {
			c.resetb()
			return
		}
		c.backend.dataRow = append(c.backend.dataRow, &pgproto3.DataRow{
			Values: func() [][]byte {
				values := make([][]byte, len(msg.Values))
				for i := range msg.Values {
					if msg.Values[i] == nil {
						values[i] = nil
					} else {
						values[i] = make([]byte, len(msg.Values[i]))
						copy(values[i], msg.Values[i])
					}
				}
				return values
			}(),
		})
	case *pgproto3.CommandComplete:
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
		if !c.frontendQuery {
			c.resetb()
			return
		}
		c.backend.commandComplete = &pgproto3.CommandComplete{
			CommandTag: make([]byte, len(msg.CommandTag)),
		}
		copy(c.backend.commandComplete.CommandTag, msg.CommandTag)
	case *pgproto3.ReadyForQuery:
		c.Mutex.Lock()
		if !c.frontendQuery {
			c.resetb()
			c.Mutex.Unlock()
			return
		} else if c.backend.commandComplete == nil { // Error query, just continue
			slog.Info("pgproxy_database_command_not_complete", "key", key(c.key, c.hook.keySuffix))
			c.resetb()
			c.Mutex.Unlock()
			return
		}
		key, val := key(c.key, c.hook.keySuffix), NewCache(c.backend.rowDescription, c.backend.dataRow, c.backend.commandComplete, msg)
		c.resetb()
		c.Mutex.Unlock()
		slog.Debug("pgproxy_cache_set", "key", key)
		if err := c.Client.Set(key, val); err != nil {
			slog.Error("pgproxy_cache_set_err", "err", err)
		}
	}
}

func (c *CacheBuffer) resetf() {
	c.frontend = buff{}
}

func (c *CacheBuffer) resetb() {
	c.frontendQuery = false
	c.key = ""
	c.hook = hook{}
	c.backend = bufb{}
}

type buff struct {
	query    *pgproto3.Query
	bind     *pgproto3.Bind
	describe *pgproto3.Describe
	execute  *pgproto3.Execute
	cached   *bufb
}

type bufb struct {
	rowDescription  *pgproto3.RowDescription
	dataRow         []*pgproto3.DataRow
	commandComplete *pgproto3.CommandComplete
	readyForQuery   *pgproto3.ReadyForQuery
}

type hook struct {
	noCache    bool
	keySuffix  string
	invalidate []string
}

func newHookFromBind(msg *pgproto3.Bind) hook {
	def := hook{
		noCache:    false,
		keySuffix:  "DEFAULT",
		invalidate: nil,
	}
	if !PGPROXY_ENABLE_PARAMETER_HOOK {
		return def
	}
	var p string
	if PGPROXY_PARAMETER_HOOK_POSITION == "first" {
		if !valid(msg.Parameters[0]) {
			return def
		}
		p = string(msg.Parameters[0])
	} else if PGPROXY_PARAMETER_HOOK_POSITION == "last" {
		if !valid(msg.Parameters[len(msg.Parameters)-1]) {
			return def
		}
		p = string(msg.Parameters[len(msg.Parameters)-1])
	} else {
		slog.Error("pgproxy_parameter_hook_position", "unknown", PGPROXY_PARAMETER_HOOK_POSITION)
		if !valid(msg.Parameters[0]) {
			return def
		}
		p = string(msg.Parameters[0])
	}
	if !strings.HasPrefix(p, "PGPROXY,") {
		slog.Error("pgproxy_parameter_hook_prefix", "invalid_prefix", p)
		return def
	}
	return def.parse(p)
}

func newHookFromQuery(_ *pgproto3.Query) hook {
	def := hook{
		noCache:    false,
		keySuffix:  "DEFAULT",
		invalidate: nil,
	}
	if !PGPROXY_ENABLE_PARAMETER_HOOK {
		return def
	}
	return def
}

func (h hook) parse(p string) hook {
	for _, config := range strings.Split(p, ",") {
		if config == "NO_CACHE" {
			h.noCache = true
		} else if strings.HasPrefix(config, "CACHE_KEY:") {
			h.keySuffix = strings.TrimPrefix(config, "CACHE_KEY:")
		} else if strings.HasPrefix(config, "INVALIDATE:") {
			h.invalidate = strings.Split(strings.TrimPrefix(config, "INVALIDATE:"), ",")
		} else {
			slog.Error("pgproxy_parameter_hook", "unknown_hook", config)
		}
	}
	return h
}

func valid(p []byte) bool {
	if !utf8.Valid(p) {
		slog.Error("pgproxy_parameter_hook_invalid", "invalid_string", base64.RawStdEncoding.EncodeToString(p))
		return false
	}
	return true
}

// Sets the assigned parameter hook into empty string
func clearHookParameter(msg *pgproto3.Bind) {
	if !PGPROXY_ENABLE_PARAMETER_HOOK {
		return
	}
	if PGPROXY_PARAMETER_HOOK_POSITION == "first" {
		if !valid(msg.Parameters[0]) {
			return
		}
		msg.Parameters[0] = []byte("")
	} else if PGPROXY_PARAMETER_HOOK_POSITION == "last" {
		if !valid(msg.Parameters[len(msg.Parameters)-1]) {
			return
		}
		msg.Parameters[len(msg.Parameters)-1] = []byte("")
	}
}

func key(key, suffix string) string {
	// TODO: might need to be able to configure the prefixed `PGPROXY` key. It is currently just
	// set so we don't conflict with keys from other system
	return fmt.Sprintf("PGPROXY:%s:%s", suffix, key)
}

func hash(b []byte) string {
	hash := sha256.New()
	hash.Write(b)
	return base64.RawStdEncoding.EncodeToString(hash.Sum(nil))
}

// When a request is cached, we'll reply with a pre-determined set of messages here
//
// Returns whether the whole request cache is valid or not. If false, we'll
// forward the whole buffered request to backend, so current query can be continued as usual
func sendCache(
	addr string,
	backend *pgproto3.Backend,
	describe *pgproto3.Describe,
	rowDescription *pgproto3.RowDescription,
	datarows []*pgproto3.DataRow,
	command *pgproto3.CommandComplete,
	ready *pgproto3.ReadyForQuery,
	errChan chan<- error,
) bool {
	backend.Send(&pgproto3.BindComplete{})
	if err := backend.Flush(); err != nil {
		terminate(errChan, errors.Join(errors.New("error when sending message to frontend (BindComplete cached)"), err))
	}
	if slog.Default().Handler().Enabled(context.Background(), slog.LevelDebug) {
		b, _ := json.Marshal(&pgproto3.BindComplete{})
		slog.Debug("B", "addr", addr, "data", b, "cached", true)
	}
	if describe != nil {
		// The incoming request contains `Describe` request, but the cache doesn't have
		// the corresponding `RowDescription` cached, so we'll just release the buffer instead
		if rowDescription == nil {
			return false
		}
		backend.Send(rowDescription)
		if err := backend.Flush(); err != nil {
			terminate(errChan, errors.Join(errors.New("error when sending message to frontend (BindComplete cached)"), err))
		}
		if slog.Default().Handler().Enabled(context.Background(), slog.LevelDebug) {
			b, _ := json.Marshal(rowDescription)
			slog.Debug("B", "addr", addr, "data", b, "cached", true)
		}
	}
	for _, msg := range datarows {
		backend.Send(msg)
		if err := backend.Flush(); err != nil {
			terminate(errChan, errors.Join(errors.New("error when sending message to frontend (DataRow cached)"), err))
		}
		if slog.Default().Handler().Enabled(context.Background(), slog.LevelDebug) {
			b, _ := json.Marshal(msg)
			slog.Debug("B", "addr", addr, "data", b, "cached", true)
		}
	}
	backend.Send(command)
	if err := backend.Flush(); err != nil {
		terminate(errChan, errors.Join(errors.New("error when sending message to frontend (CommandComplete cached)"), err))
	}
	if slog.Default().Handler().Enabled(context.Background(), slog.LevelDebug) {
		b, _ := json.Marshal(command)
		slog.Debug("B", "addr", addr, "data", b, "cached", true)
	}
	backend.Send(ready)
	if err := backend.Flush(); err != nil {
		terminate(errChan, errors.Join(errors.New("error when sending message to frontend (ReadyForQuery cached)"), err))
	}
	if slog.Default().Handler().Enabled(context.Background(), slog.LevelDebug) {
		b, _ := json.Marshal(ready)
		slog.Debug("B", "addr", addr, "data", b, "cached", true)
	}
	return true
}

func invalidate(client CacheI, keys []string) {
	for _, key := range keys {
		slog.Debug("pgproxy_cache_del", "key", key)
		go client.DelPattern(key)
	}
}

type Cache struct {
	RowDescription  *string  `json:"row_description,omitempty"`
	DataRow         []string `json:"data_row"`
	CommandComplete string   `json:"command_complete"`
	ReadyForQuery   string   `json:"ready_for_query"`
}

func NewCache(
	rd *pgproto3.RowDescription,
	dr []*pgproto3.DataRow,
	cc *pgproto3.CommandComplete,
	rfq *pgproto3.ReadyForQuery,
) Cache {
	var c Cache
	if rd == nil {
		c.RowDescription = nil
	} else {
		e, err := rd.Encode(nil)
		if err != nil {
			panic(err)
		}
		c.RowDescription = ref(base64.StdEncoding.EncodeToString(e))
	}
	c.DataRow = make([]string, len(dr))
	for i := range dr {
		e, err := dr[i].Encode(nil)
		if err != nil {
			panic(err)
		}
		c.DataRow[i] = base64.StdEncoding.EncodeToString(e)
	}
	{
		e, err := cc.Encode(nil)
		if err != nil {
			panic(err)
		}
		c.CommandComplete = base64.StdEncoding.EncodeToString(e)
	}
	{
		e, err := rfq.Encode(nil)
		if err != nil {
			panic(err)
		}
		c.ReadyForQuery = base64.StdEncoding.EncodeToString(e)
	}
	return c
}

func (c Cache) ToCommand() (
	*pgproto3.RowDescription,
	[]*pgproto3.DataRow,
	*pgproto3.CommandComplete,
	*pgproto3.ReadyForQuery,
) {
	rd, dr, cc, rfq := new(pgproto3.RowDescription), make([]*pgproto3.DataRow, 0), new(pgproto3.CommandComplete), new(pgproto3.ReadyForQuery)
	if c.RowDescription == nil {
		rd = nil
	} else {
		d, err := base64.StdEncoding.DecodeString(*c.RowDescription)
		if err != nil {
			panic(err)
		}
		// Skip the first 5 bytes -- `Decode` expects the message to not contain the 1 byte message type and 4 byte length
		if err := rd.Decode(d[5:]); err != nil {
			panic(err)
		}
	}
	for i := range c.DataRow {
		d, err := base64.StdEncoding.DecodeString(c.DataRow[i])
		if err != nil {
			panic(err)
		}
		msg := new(pgproto3.DataRow)
		if err := msg.Decode(d[5:]); err != nil {
			panic(err)
		}
		dr = append(dr, msg)
	}
	{
		d, err := base64.StdEncoding.DecodeString(c.CommandComplete)
		if err != nil {
			panic(err)
		}
		if err := cc.Decode(d[5:]); err != nil {
			panic(err)
		}
	}
	{
		d, err := base64.StdEncoding.DecodeString(c.ReadyForQuery)
		if err != nil {
			panic(err)
		}
		if err := rfq.Decode(d[5:]); err != nil {
			panic(err)
		}
	}
	return rd, dr, cc, rfq
}

type CacheRedis struct {
	client *redis.Client
}

func NewCacheRedis(client *redis.Client) *CacheRedis {
	return &CacheRedis{client: client}
}

func (c *CacheRedis) Get(key string) (*Cache, bool) {
	// This context is a bit iffy -- a timed-out connection would just be closed immediately
	// by the apps or database, so we'll just put 1 second here so at least this doesn't get to
	// run for too long
	ctx, cancel := context.WithTimeout(context.Background(), (1 * time.Second))
	defer cancel()
	b, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if err != redis.Nil {
			slog.Error("pgproxy_cache_get", "err", err)
		}
		return nil, false
	}
	var decoded Cache
	if err := json.Unmarshal(b, &decoded); err != nil {
		slog.Error("pgproxy_cache_get_decode", "err", err)
		return nil, false
	}
	return &decoded, true
}

func (c *CacheRedis) Set(key string, val Cache) error {
	ttl := func(ttl, jitter int64) time.Duration {
		jitter = int64(rand.Intn(max(int(jitter), 1)))
		return time.Duration(ttl+jitter) * time.Second
	}
	b, err := json.Marshal(val)
	if err != nil {
		panic(err) // Should not happen unless the cached data is manually mucked around
	}
	// This context is a bit iffy -- a timed-out connection would just be closed immediately
	// by the apps or database, so we'll just put 1 second here so at least this doesn't get to
	// run for too long
	ctx, cancel := context.WithTimeout(context.Background(), (1 * time.Second))
	defer cancel()
	return c.client.SetEx(ctx, key, b, ttl(PGPROXY_CACHE_TTL_SECOND, PGPROXY_CACHE_TTL_JITTER_SECOND)).Err()
}

func (c *CacheRedis) DelPattern(key string) {
	cursor, total, keys := uint64(0), uint64(0), make([]string, 0)
	for {
		k, c, err := c.client.Scan(context.Background(), cursor, fmt.Sprintf("*%s*", key), 1000).Result()
		if err != nil {
			slog.Error("pgproxy_cache_del_scan", "err", err)
			return
		}
		total, keys = (total + uint64(len(k))), append(keys, k...)
		if c == 0 {
			break
		}
		cursor = c
	}
	if err := c.client.Del(context.Background(), keys...).Err(); err != nil {
		slog.Error("pgproxy_cache_del", "err", err)
	}
}

type CacheMemory struct {
	*sync.Mutex
	data map[string]struct {
		ttl time.Time
		val Cache
	}
}

func NewCacheMemory() *CacheMemory {
	mem := &CacheMemory{Mutex: new(sync.Mutex), data: map[string]struct {
		ttl time.Time
		val Cache
	}{}}
	go mem.gc()
	return mem
}

func (c *CacheMemory) Get(key string) (*Cache, bool) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()
	val, ok := c.data[key]
	if !ok {
		return nil, false
	}
	return &val.val, true
}

func (c *CacheMemory) Set(key string, val Cache) error {
	ttl := func(ttl, jitter int64) time.Duration {
		jitter = int64(rand.Intn(max(int(jitter), 1)))
		return time.Duration(ttl+jitter) * time.Second
	}
	c.data[key] = struct {
		ttl time.Time
		val Cache
	}{
		ttl: time.Now().Add(ttl(PGPROXY_CACHE_TTL_SECOND, PGPROXY_CACHE_TTL_JITTER_SECOND)),
		val: val,
	}
	return nil
}

func (c *CacheMemory) DelPattern(key string) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()
	for k := range c.data {
		if strings.Contains(k, key) {
			delete(c.data, k)
		}
	}
}

func (c *CacheMemory) gc() {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()
	now := time.Now()
	for k, v := range c.data {
		if v.ttl.Before(now) {
			delete(c.data, k)
		}
	}
}

func ref[T any](v T) *T {
	return &v
}
