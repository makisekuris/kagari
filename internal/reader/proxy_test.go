package reader

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserProxyRejectsPrivateDestinationsAndRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/metadata", http.StatusFound)
			return
		}
		w.Write([]byte("private content"))
	}))
	defer target.Close()
	proxy, close, err := StartBrowserProxy(context.Background(), Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	u, _ := url.Parse(proxy)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: time.Second}
	for _, address := range []string{target.URL, "https://127.0.0.1:443"} {
		response, err := client.Get(address)
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadGateway {
				t.Fatalf("private destination accepted: %s %s", address, body)
			}
		}
	}
	proxyAllowed, closeAllowed, err := StartBrowserProxy(context.Background(), Options{Timeout: time.Second, AllowedNonPublicCIDRs: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAllowed()
	u, _ = url.Parse(proxyAllowed)
	client.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("explicit development CIDR was rejected")
	}
	redirect, err := client.Get(target.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer redirect.Body.Close()
	if redirect.StatusCode != http.StatusBadGateway {
		t.Fatal("redirect escaped destination policy")
	}
}

func TestBrowserProxyProtectsHTTPSConnect(t *testing.T) {
	var visits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { visits.Add(1); w.Write([]byte("TLS evidence")) }))
	defer target.Close()
	proxy, close, err := StartBrowserProxy(context.Background(), Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	u, _ := url.Parse(proxy)
	transport := target.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(u)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	if response, err := client.Get(target.URL); err == nil {
		response.Body.Close()
		t.Fatal("private HTTPS tunnel accepted")
	}
	if visits.Load() != 0 {
		t.Fatal("rejected CONNECT reached private TLS server")
	}
	proxyAllowed, closeAllowed, err := StartBrowserProxy(context.Background(), Options{Timeout: time.Second, AllowedNonPublicCIDRs: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAllowed()
	u, _ = url.Parse(proxyAllowed)
	transport = target.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(u)
	client.Transport = transport
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || visits.Load() != 1 {
		t.Fatal("explicitly permitted HTTPS request did not reach TLS server")
	}
}
