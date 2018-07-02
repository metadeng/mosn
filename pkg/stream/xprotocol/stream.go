package xprotocol

import (
	"context"
	"sync"
	
	"gitlab.alipay-inc.com/afe/mosn/pkg/log"
	"gitlab.alipay-inc.com/afe/mosn/pkg/protocol"
	str "gitlab.alipay-inc.com/afe/mosn/pkg/stream"
	"gitlab.alipay-inc.com/afe/mosn/pkg/types"
	"sync/atomic"
	"strconv"
)

type StreamDirection int
var streamIdXprotocolCount uint32

const (
	ServerStream StreamDirection = 1
	ClientStream StreamDirection = 0
)

func init() {
	str.Register(protocol.Xprotocol, &streamConnFactory{})
}

type streamConnFactory struct{}

func (f *streamConnFactory) CreateClientStream(context context.Context, connection types.ClientConnection,
	clientCallbacks types.StreamConnectionEventListener, connCallbacks types.ConnectionEventListener) types.ClientStreamConnection {
	return newStreamConnection(context, connection, clientCallbacks, nil)
}

func (f *streamConnFactory) CreateServerStream(context context.Context, connection types.Connection,
	serverCallbacks types.ServerStreamConnectionEventListener) types.ServerStreamConnection {
	return newStreamConnection(context, connection, nil, serverCallbacks)
}

func (f *streamConnFactory) CreateBiDirectStream(context context.Context, connection types.ClientConnection,
	clientCallbacks types.StreamConnectionEventListener,
	serverCallbacks types.ServerStreamConnectionEventListener) types.ClientStreamConnection {
	return newStreamConnection(context, connection, clientCallbacks, serverCallbacks)
}

// types.DecodeFilter
// types.StreamConnection
// types.ClientStreamConnection
// types.ServerStreamConnection
type streamConnection struct {
	context         context.Context
	protocol        types.Protocol
	connection      types.Connection
	activeStream    streamMap
	clientCallbacks types.StreamConnectionEventListener
	serverCallbacks types.ServerStreamConnectionEventListener

	logger log.Logger
}

func newStreamConnection(context context.Context, connection types.Connection, clientCallbacks types.StreamConnectionEventListener,
	serverCallbacks types.ServerStreamConnectionEventListener) types.ClientStreamConnection {

	return &streamConnection{
		context:         context,
		connection:      connection,
		activeStream:    newStreamMap(context),
		clientCallbacks: clientCallbacks,
		serverCallbacks: serverCallbacks,
		logger:          log.ByContext(context),
	}
}

// types.StreamConnection
func (conn *streamConnection) Dispatch(buffer types.IoBuffer) {
	log.StartLogger.Debugf("stream connection dispatch data = %v", buffer.String())
	streamId := ""
	if conn.serverCallbacks != nil {
		reqId := atomic.AddUint32(&streamIdXprotocolCount, 1)
		streamId = strconv.FormatUint(uint64(reqId), 10)
	}
	headers := make(map[string]string)
	// support dynamic route
	headers["Host"] = conn.connection.RemoteAddr().String()
	headers["Path"] = "/"
	log.StartLogger.Debugf("before Dispatch on decode header")
	conn.OnDecodeHeader(streamId, headers)
	log.StartLogger.Debugf("after Dispatch on decode header")
	conn.OnDecodeData(streamId, buffer)
	log.StartLogger.Debugf("after Dispatch on decode data")
}

func (conn *streamConnection) Protocol() types.Protocol {
	return conn.protocol
}

func (conn *streamConnection) GoAway() {
	// todo
}

func (conn *streamConnection) OnUnderlyingConnectionAboveWriteBufferHighWatermark() {
	// todo
}

func (conn *streamConnection) OnUnderlyingConnectionBelowWriteBufferLowWatermark() {
	// todo
}

func (conn *streamConnection) NewStream(streamId string, responseDecoder types.StreamDecoder) types.StreamEncoder {
	log.StartLogger.Debugf("xprotocol stream new stream")
	stream := stream{
		context:    context.WithValue(conn.context, types.ContextKeyStreamId, streamId),
		streamId:   streamId,
		direction:  ClientStream,
		connection: conn,
		decoder:    responseDecoder,
	}
	conn.activeStream.Set(streamId, stream)

	return &stream
}

func (conn *streamConnection) OnDecodeHeader(streamId string, headers map[string]string) types.FilterStatus {
	log.StartLogger.Debugf("xprotocol stream on decode header")
	if conn.serverCallbacks != nil {
		log.StartLogger.Debugf("xprotocol stream on new stream deteced invoked")
		conn.onNewStreamDetected(streamId, headers)
	}
	if stream, ok := conn.activeStream.Get(streamId); ok {
		log.StartLogger.Debugf("before stream decoder invoke on decode header")
		stream.decoder.OnDecodeHeaders(headers, false)
	}
	log.StartLogger.Debugf("after stream decoder invoke on decode header")
	return types.Continue
}

func (conn *streamConnection) OnDecodeData(streamId string, data types.IoBuffer) types.FilterStatus {
	if stream, ok := conn.activeStream.Get(streamId); ok {
		log.StartLogger.Debugf("xprotocol stream on decode data")
		stream.decoder.OnDecodeData(data, true)

		if stream.direction == ClientStream {
			// for client stream, remove stream on response read
			stream.connection.activeStream.Remove(stream.streamId)
		}
	}
	data.Drain(data.Len())
	return types.StopIteration
}

func (conn *streamConnection) OnDecodeTrailer(streamId string, trailers map[string]string) types.FilterStatus {
	// unsupported
	return types.StopIteration
}

// todo, deal with more exception
func (conn *streamConnection) OnDecodeError(err error, header map[string]string) {
	if err == nil {
		return
	}
	// for header decode error, close the connection directly
	if err.Error() == types.UnSupportedProCode {
		conn.connection.Close(types.NoFlush, types.LocalClose)
	}
}

func (conn *streamConnection) onNewStreamDetected(streamId string, headers map[string]string) {
	if ok := conn.activeStream.Has(streamId); ok {
		return
	}
	stream := stream{
		context:    context.WithValue(conn.context, types.ContextKeyStreamId, streamId),
		streamId:   streamId,
		direction:  ServerStream,
		connection: conn,
	}

	stream.decoder = conn.serverCallbacks.NewStream(streamId, &stream)
	conn.activeStream.Set(streamId, stream)
}

// types.Stream
// types.StreamEncoder
type stream struct {
	context context.Context

	streamId         string
	direction        StreamDirection // 0: out, 1: in
	readDisableCount int
	connection       *streamConnection
	decoder          types.StreamDecoder
	streamCbs        []types.StreamEventListener
	encodedHeaders   types.IoBuffer
	encodedData      types.IoBuffer
}

// ~~ types.Stream
func (s *stream) AddEventListener(cb types.StreamEventListener) {
	s.streamCbs = append(s.streamCbs, cb)
}

func (s *stream) RemoveEventListener(cb types.StreamEventListener) {
	cbIdx := -1

	for i, streamCb := range s.streamCbs {
		if streamCb == cb {
			cbIdx = i
			break
		}
	}

	if cbIdx > -1 {
		s.streamCbs = append(s.streamCbs[:cbIdx], s.streamCbs[cbIdx+1:]...)
	}
}

func (s *stream) ResetStream(reason types.StreamResetReason) {
	for _, cb := range s.streamCbs {
		cb.OnResetStream(reason)
	}
}

func (s *stream) ReadDisable(disable bool) {
	s.connection.connection.SetReadDisable(disable)
}

func (s *stream) BufferLimit() uint32 {
	return s.connection.connection.BufferLimit()
}

// types.StreamEncoder
func (s *stream) EncodeHeaders(headers interface{}, endStream bool) error {
	log.StartLogger.Debugf("EncodeHeaders,request id = %s, direction = %d",s.streamId,s.direction)
	if endStream {
		s.endStream()
	}
	return nil
}

func (s *stream) EncodeData(data types.IoBuffer, endStream bool) error {
	s.encodedData = data
	log.StartLogger.Debugf("EncodeData,request id = %s, direction = %d,data = %v",s.streamId,s.direction,data.String())
	if endStream {
		s.endStream()
	}
	return nil
}

func (s *stream) EncodeTrailers(trailers map[string]string) error {
	log.StartLogger.Debugf("EncodeTrailers,request id = %s, direction = %d",s.streamId,s.direction)
	s.endStream()
	return nil
}

// Flush stream data
// For server stream, write out response
// For client stream, write out request

//TODO: x-protocol stream has encodeHeaders?
func (s *stream) endStream() {
	log.StartLogger.Debugf("xprotocol stream end stream invoked , request id = %s, direction = %d",s.streamId,s.direction)
	if stream, ok := s.connection.activeStream.Get(s.streamId); ok {
		log.StartLogger.Debugf("xprotocol stream end stream write encodedata")
		stream.connection.connection.Write(s.encodedData)
	} else {
		s.connection.logger.Errorf("No stream %s to end", s.streamId)
	}

	if s.direction == ServerStream {
		// for a server stream, remove stream on response wrote
		s.connection.activeStream.Remove(s.streamId)
		log.StartLogger.Warnf("Remove Request ID = %+v",s.streamId)
	}
}

func (s *stream) GetStream() types.Stream {
	return s
}

type streamMap struct {
	smap map[string]interface{}
	mux  sync.RWMutex
}

func newStreamMap(context context.Context) streamMap {
	smap := str.GetMap(context, 5096)

	return streamMap{
		smap: smap,
	}
}

func (m *streamMap) Has(streamId string) bool {
	m.mux.RLock()
	defer m.mux.RUnlock()

	if _, ok := m.smap[streamId]; ok {
		return true
	} else {
		return false
	}
}

func (m *streamMap) Get(streamId string) (stream, bool) {
	m.mux.RLock()
	defer m.mux.RUnlock()

	if s, ok := m.smap[streamId]; ok {
		return s.(stream), ok
	} else {
		return stream{}, false
	}
}

func (m *streamMap) Remove(streamId string) {
	m.mux.Lock()
	defer m.mux.Unlock()

	delete(m.smap, streamId)
}

func (m *streamMap) Set(streamId string, s stream) {
	m.mux.Lock()
	defer m.mux.Unlock()

	m.smap[streamId] = s
}
