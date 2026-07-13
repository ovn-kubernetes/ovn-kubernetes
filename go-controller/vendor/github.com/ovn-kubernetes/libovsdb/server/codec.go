package server

import (
	"sync"

	"github.com/cenkalti/rpc2"
)

// serialCodec wraps an rpc2.Codec so that WriteRequest and WriteResponse are
// serialized. The jsonrpc codec's Encode is not safe for concurrent use by
// multiple handler goroutines; this wrapper satisfies the Codec contract.
type serialCodec struct {
	mu sync.Mutex
	c  rpc2.Codec
}

func newSerialCodec(c rpc2.Codec) rpc2.Codec {
	return &serialCodec{c: c}
}

func (s *serialCodec) ReadHeader(req *rpc2.Request, resp *rpc2.Response) error {
	return s.c.ReadHeader(req, resp)
}

func (s *serialCodec) ReadRequestBody(x interface{}) error {
	return s.c.ReadRequestBody(x)
}

func (s *serialCodec) ReadResponseBody(x interface{}) error {
	return s.c.ReadResponseBody(x)
}

func (s *serialCodec) WriteRequest(r *rpc2.Request, x interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.WriteRequest(r, x)
}

func (s *serialCodec) WriteResponse(r *rpc2.Response, x interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.WriteResponse(r, x)
}

func (s *serialCodec) Close() error {
	return s.c.Close()
}
