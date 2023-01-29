package local

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/util/forward"
	"github.com/go-gost/x/registry"
)

func init() {
	registry.HandlerRegistry().Register("tcp", NewHandler)
	registry.HandlerRegistry().Register("udp", NewHandler)
	registry.HandlerRegistry().Register("forward", NewHandler)
}

type forwardHandler struct {
	hop     chain.Hop
	router  *chain.Router
	md      metadata
	options handler.Options
}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := handler.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	return &forwardHandler{
		options: options,
	}
}

func (h *forwardHandler) Init(md md.Metadata) (err error) {
	if err = h.parseMetadata(md); err != nil {
		return
	}

	h.router = h.options.Router
	if h.router == nil {
		h.router = chain.NewRouter(chain.LoggerRouterOption(h.options.Logger))
	}

	return
}

// Forward implements handler.Forwarder.
func (h *forwardHandler) Forward(hop chain.Hop) {
	h.hop = hop
}

func (h *forwardHandler) Handle(ctx context.Context, conn net.Conn, opts ...handler.HandleOption) error {
	defer conn.Close()

	start := time.Now()
	log := h.options.Logger.WithFields(map[string]any{
		"remote": conn.RemoteAddr().String(),
		"local":  conn.LocalAddr().String(),
	})

	log.Infof("%s <> %s", conn.RemoteAddr(), conn.LocalAddr())
	defer func() {
		log.WithFields(map[string]any{
			"duration": time.Since(start),
		}).Infof("%s >< %s", conn.RemoteAddr(), conn.LocalAddr())
	}()

	if !h.checkRateLimit(conn.RemoteAddr()) {
		return nil
	}

	network := "tcp"
	if _, ok := conn.(net.PacketConn); ok {
		network = "udp"
	}

	var rw io.ReadWriter = conn
	var host string
	var protocol string
	if h.md.sniffing {
		if network == "tcp" {
			rw, host, protocol, _ = forward.Sniffing(ctx, conn)
			h.options.Logger.Debugf("sniffing: host=%s, protocol=%s", host, protocol)
		}
	}

	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "0")
	}
	target := &chain.Node{
		Addr: host,
	}
	if h.hop != nil {
		target = h.hop.Select(ctx,
			chain.HostSelectOption(host),
			chain.ProtocolSelectOption(protocol),
		)
		if target == nil {
			err := errors.New("target not available")
			log.Error(err)
			return err
		}
	}

	log = log.WithFields(map[string]any{
		"dst": fmt.Sprintf("%s/%s", target.Addr, network),
	})

	log.Debugf("%s >> %s", conn.RemoteAddr(), target.Addr)

	cc, err := h.router.Dial(ctx, network, target.Addr)
	if err != nil {
		log.Error(err)
		// TODO: the router itself may be failed due to the failed node in the router,
		// the dead marker may be a wrong operation.
		if marker := target.Marker(); marker != nil {
			marker.Mark()
		}
		return err
	}
	defer cc.Close()
	if marker := target.Marker(); marker != nil {
		marker.Reset()
	}

	t := time.Now()
	log.Debugf("%s <-> %s", conn.RemoteAddr(), target.Addr)

	if protocol == forward.ProtoHTTP &&
		target.Options().HTTP != nil {
		h.handleHTTP(ctx, rw, cc, target.Options().HTTP, log)
	} else {
		xnet.Transport(rw, cc)
	}

	log.WithFields(map[string]any{
		"duration": time.Since(t),
	}).Debugf("%s >-< %s", conn.RemoteAddr(), target.Addr)

	return nil
}

func (h *forwardHandler) checkRateLimit(addr net.Addr) bool {
	if h.options.RateLimiter == nil {
		return true
	}
	host, _, _ := net.SplitHostPort(addr.String())
	if limiter := h.options.RateLimiter.Limiter(host); limiter != nil {
		return limiter.Allow(1)
	}

	return true
}

func (h *forwardHandler) handleHTTP(ctx context.Context, src, dst io.ReadWriter, httpSettings *chain.HTTPNodeSettings, log logger.Logger) error {
	errc := make(chan error, 1)
	go func() {
		errc <- xnet.CopyBuffer(src, dst, 8192)
	}()

	go func() {
		br := bufio.NewReader(src)
		for {
			err := func() error {
				req, err := http.ReadRequest(br)
				if err != nil {
					return err
				}

				if httpSettings.Host != "" {
					req.Host = httpSettings.Host
				}
				for k, v := range httpSettings.Header {
					req.Header.Set(k, v)
				}

				if log.IsLevelEnabled(logger.TraceLevel) {
					dump, _ := httputil.DumpRequest(req, false)
					log.Trace(string(dump))
				}
				if err := req.Write(dst); err != nil {
					return err
				}

				if req.Header.Get("Upgrade") == "websocket" {
					err := xnet.CopyBuffer(dst, src, 8192)
					if err == nil {
						err = io.EOF
					}
					return err
				}
				return nil
			}()
			if err != nil {
				errc <- err
				break
			}
		}
	}()

	if err := <-errc; err != nil && err != io.EOF {
		return err
	}

	return nil
}
