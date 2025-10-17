package pgproxy

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgproto3"
)

var ErrInsufficientHeaderLength error = errors.New("invalid message: insufficient header length")

type FrontendMessageType byte

const (
	FRONTEND_MESSAGE_TYPE_BIND          FrontendMessageType = 'B'
	FRONTEND_MESSAGE_TYPE_CLOSE         FrontendMessageType = 'C'
	FRONTEND_MESSAGE_TYPE_DESCRIBE      FrontendMessageType = 'D'
	FRONTEND_MESSAGE_TYPE_EXECUTE       FrontendMessageType = 'E'
	FRONTEND_MESSAGE_TYPE_FUNCTION_CALL FrontendMessageType = 'F'
	FRONTEND_MESSAGE_TYPE_COPY_FAIL     FrontendMessageType = 'f'
	FRONTEND_MESSAGE_TYPE_COPY_DATA     FrontendMessageType = 'd'
	FRONTEND_MESSAGE_TYPE_COPY_DONE     FrontendMessageType = 'c'
	FRONTEND_MESSAGE_TYPE_FLUSH         FrontendMessageType = 'H'
	FRONTEND_MESSAGE_TYPE_PARSE         FrontendMessageType = 'P'
	FRONTEND_MESSAGE_TYPE_QUERY         FrontendMessageType = 'Q'
	FRONTEND_MESSAGE_TYPE_SYNC          FrontendMessageType = 'S'
	FRONTEND_MESSAGE_TYPE_TERMINATE     FrontendMessageType = 'X'
)

// Parse `b` into `pgproto3.FrontendMessage`. Caller can reflect with concrete type for specific message data.
// Can result in multiple messages being parsed.
func NewPgxFrontendMessage(b []byte) ([]pgproto3.FrontendMessage, error) {
	if len(b) < 5 {
		return nil, fmt.Errorf("%w: length %d", ErrInsufficientHeaderLength, len(b))
	}
	header, length := FrontendMessageType(b[0]), int(binary.BigEndian.Uint32(b[1:5]))
	var msg pgproto3.FrontendMessage
	switch header {
	case FRONTEND_MESSAGE_TYPE_BIND:
		msg = new(pgproto3.Bind)
	case FRONTEND_MESSAGE_TYPE_CLOSE:
		msg = new(pgproto3.Close)
	case FRONTEND_MESSAGE_TYPE_COPY_DATA:
		msg = new(pgproto3.CopyData)
	case FRONTEND_MESSAGE_TYPE_COPY_DONE:
		msg = new(pgproto3.CopyDone)
	case FRONTEND_MESSAGE_TYPE_COPY_FAIL:
		msg = new(pgproto3.CopyFail)
	case FRONTEND_MESSAGE_TYPE_DESCRIBE:
		msg = new(pgproto3.Describe)
	case FRONTEND_MESSAGE_TYPE_EXECUTE:
		msg = new(pgproto3.Execute)
	case FRONTEND_MESSAGE_TYPE_FLUSH:
		msg = new(pgproto3.Flush)
	case FRONTEND_MESSAGE_TYPE_FUNCTION_CALL:
		msg = new(pgproto3.FunctionCall)
	case FRONTEND_MESSAGE_TYPE_PARSE:
		msg = new(pgproto3.Parse)
	case FRONTEND_MESSAGE_TYPE_QUERY:
		msg = new(pgproto3.Query)
	case FRONTEND_MESSAGE_TYPE_SYNC:
		msg = new(pgproto3.Sync)
	case FRONTEND_MESSAGE_TYPE_TERMINATE:
		msg = new(pgproto3.Terminate)
	default:
		return nil, nil
	}
	if err := msg.Decode(b[5 : length+1]); err != nil {
		return nil, err
	}
	if length == len(b[1:]) {
		return []pgproto3.FrontendMessage{msg}, nil
	} else {
		msg2, err := NewPgxFrontendMessage(b[length+1:])
		return append([]pgproto3.FrontendMessage{msg}, msg2...), err
	}
}

type BackendMessageType byte

const (
	BACKEND_MESSAGE_TYPE_PARSE_COMPLETE         BackendMessageType = '1'
	BACKEND_MESSAGE_TYPE_BIND_COMPLETE          BackendMessageType = '2'
	BACKEND_MESSAGE_TYPE_CLOSE_COMPLETE         BackendMessageType = '3'
	BACKEND_MESSAGE_TYPE_NOTIFICATION_RESPONSE  BackendMessageType = 'A'
	BACKEND_MESSAGE_TYPE_COPY_DONE              BackendMessageType = 'c'
	BACKEND_MESSAGE_TYPE_COMMAND_COMPLETE       BackendMessageType = 'C'
	BACKEND_MESSAGE_TYPE_COPY_DATA              BackendMessageType = 'd'
	BACKEND_MESSAGE_TYPE_DATA_ROW               BackendMessageType = 'D'
	BACKEND_MESSAGE_TYPE_ERROR_RESPONSE         BackendMessageType = 'E'
	BACKEND_MESSAGE_TYPE_COPY_IN_RESPONSE       BackendMessageType = 'G'
	BACKEND_MESSAGE_TYPE_COPY_OUT_RESPONSE      BackendMessageType = 'H'
	BACKEND_MESSAGE_TYPE_EMPTY_QUERY_RESPONSE   BackendMessageType = 'I'
	BACKEND_MESSAGE_TYPE_BACKEND_KEY_DATA       BackendMessageType = 'K'
	BACKEND_MESSAGE_TYPE_NO_DATA                BackendMessageType = 'n'
	BACKEND_MESSAGE_TYPE_NOTICE_RESPONSE        BackendMessageType = 'N'
	BACKEND_MESSAGE_TYPE_PORTAL_SUSPENDED       BackendMessageType = 's'
	BACKEND_MESSAGE_TYPE_PARAMETER_STATUS       BackendMessageType = 'S'
	BACKEND_MESSAGE_TYPE_PARAMETER_DESCRIPTION  BackendMessageType = 't'
	BACKEND_MESSAGE_TYPE_ROW_DESCRIPTION        BackendMessageType = 'T'
	BACKEND_MESSAGE_TYPE_FUNCTION_CALL_RESPONSE BackendMessageType = 'V'
	BACKEND_MESSAGE_TYPE_COPY_BOTH_RESPONSE     BackendMessageType = 'W'
	BACKEND_MESSAGE_TYPE_READY_FOR_QUERY        BackendMessageType = 'Z'
)

// Parse `b` into `pgproto3.BackendMessage`. Caller can reflect with concrete type for specific message data.
// Can result in multiple messages being parsed.
func NewPgxBackendMessage(b []byte) ([]pgproto3.BackendMessage, error) {
	if len(b) < 5 {
		return nil, fmt.Errorf("%w: length %d", ErrInsufficientHeaderLength, len(b))
	}
	header, length := BackendMessageType(b[0]), int(binary.BigEndian.Uint32(b[1:]))
	var msg pgproto3.BackendMessage
	switch header {
	case BACKEND_MESSAGE_TYPE_BACKEND_KEY_DATA:
		msg = new(pgproto3.BackendKeyData)
	case BACKEND_MESSAGE_TYPE_BIND_COMPLETE:
		msg = new(pgproto3.BindComplete)
	case BACKEND_MESSAGE_TYPE_CLOSE_COMPLETE:
		msg = new(pgproto3.CloseComplete)
	case BACKEND_MESSAGE_TYPE_COMMAND_COMPLETE:
		msg = new(pgproto3.CommandComplete)
	case BACKEND_MESSAGE_TYPE_COPY_BOTH_RESPONSE:
		msg = new(pgproto3.CopyBothResponse)
	case BACKEND_MESSAGE_TYPE_COPY_DATA:
		msg = new(pgproto3.CopyData)
	case BACKEND_MESSAGE_TYPE_COPY_DONE:
		msg = new(pgproto3.CopyDone)
	case BACKEND_MESSAGE_TYPE_COPY_IN_RESPONSE:
		msg = new(pgproto3.CopyInResponse)
	case BACKEND_MESSAGE_TYPE_COPY_OUT_RESPONSE:
		msg = new(pgproto3.CopyOutResponse)
	case BACKEND_MESSAGE_TYPE_DATA_ROW:
		msg = new(pgproto3.DataRow)
	case BACKEND_MESSAGE_TYPE_EMPTY_QUERY_RESPONSE:
		msg = new(pgproto3.EmptyQueryResponse)
	case BACKEND_MESSAGE_TYPE_ERROR_RESPONSE:
		msg = new(pgproto3.ErrorResponse)
	case BACKEND_MESSAGE_TYPE_FUNCTION_CALL_RESPONSE:
		msg = new(pgproto3.FunctionCallResponse)
	case BACKEND_MESSAGE_TYPE_NOTICE_RESPONSE:
		msg = new(pgproto3.NoticeResponse)
	case BACKEND_MESSAGE_TYPE_NOTIFICATION_RESPONSE:
		msg = new(pgproto3.NotificationResponse)
	case BACKEND_MESSAGE_TYPE_NO_DATA:
		msg = new(pgproto3.NoData)
	case BACKEND_MESSAGE_TYPE_PARAMETER_DESCRIPTION:
		msg = new(pgproto3.ParameterDescription)
	case BACKEND_MESSAGE_TYPE_PARAMETER_STATUS:
		msg = new(pgproto3.ParameterStatus)
	case BACKEND_MESSAGE_TYPE_PARSE_COMPLETE:
		msg = new(pgproto3.ParseComplete)
	case BACKEND_MESSAGE_TYPE_PORTAL_SUSPENDED:
		msg = new(pgproto3.PortalSuspended)
	case BACKEND_MESSAGE_TYPE_READY_FOR_QUERY:
		msg = new(pgproto3.ReadyForQuery)
	case BACKEND_MESSAGE_TYPE_ROW_DESCRIPTION:
		msg = new(pgproto3.RowDescription)
	default:
		return nil, nil
	}
	if err := msg.Decode(b[5 : length+1]); err != nil {
		return nil, err
	}
	if length == len(b[1:]) {
		return []pgproto3.BackendMessage{msg}, nil
	} else {
		msg2, err := NewPgxBackendMessage(b[length+1:])
		return append([]pgproto3.BackendMessage{msg}, msg2...), err
	}
}
