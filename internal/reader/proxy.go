package reader

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// StartBrowserProxy routes browser navigation, redirects and subresources through
// the same IP-pinned DNS policy as HTTP reads. Playwright origin filters alone do
// not protect redirects or DNS rebinding.
func StartBrowserProxy(ctx context.Context, opts Options) (string, func(), error) {
	opts = defaults(opts)
	transport := safeTransport(opts).(*http.Transport)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{ReadHeaderTimeout: opts.Timeout, BaseContext: func(net.Listener) context.Context { return ctx }}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect {
			upstream, err := transport.DialContext(req.Context(), "tcp", req.Host)
			if err != nil {
				http.Error(w, "browser destination rejected or unavailable", http.StatusBadGateway)
				return
			}
			hijack, ok := w.(http.Hijacker)
			if !ok {
				_ = upstream.Close()
				http.Error(w, "tunnel unavailable", http.StatusInternalServerError)
				return
			}
			client, buffered, err := hijack.Hijack()
			if err != nil {
				_ = upstream.Close()
				return
			}
			defer client.Close()
			defer upstream.Close()
			_ = client.SetDeadline(time.Now().Add(opts.Timeout))
			_ = upstream.SetDeadline(time.Now().Add(opts.Timeout))
			if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				return
			}
			if err := buffered.Flush(); err != nil {
				return
			}
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, buffered); done <- struct{}{} }()
			go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
			select {
			case <-done:
			case <-ctx.Done():
			}
			return
		}
		if req.URL.Scheme != "http" || req.URL.Host == "" || req.URL.User != nil {
			http.Error(w, "invalid proxy request", http.StatusBadRequest)
			return
		}
		forward := req.Clone(req.Context())
		forward.RequestURI = ""
		forward.Header.Del("Proxy-Authorization")
		forward.Header.Del("Proxy-Connection")
		response, err := transport.RoundTrip(forward)
		if err != nil {
			http.Error(w, "browser destination rejected or unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	})
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = listener.Close()
		}
	}()
	close := func() { _ = server.Close(); transport.CloseIdleConnections() }
	return "http://" + listener.Addr().String(), close, nil
}
