// This file adds the smart router first-byte deadline probe: a slow upstream, one that sends headers and a
// keep-alive comment and then stalls, and a router whose every model is slow, interpreted under Yaegi.
//
// It builds its OWN Gateway so no other probe's traffic or state reaches it.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
)

const (
	fbOK      = "fb-ok"
	fbDead    = "fb-dead"
	fbTimeout = "1s"
	// fbStall outlasts every probe request; handlers return as soon as the gateway cancels the attempt.
	fbStall = 20 * time.Second
	// fbBudget is far above one deadline and far below fbStall: an escalated answer arrives well inside it.
	fbBudget = 8 * time.Second
	fbChunk  = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
)

// fbSlowUpstream drains the request, then stalls until the client goes away. A stream request gets
// headers and a comment line first, so only the first data event is late.
func fbSlowUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(": keep-alive\n\n"))
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(fbStall):
		}
	}))
}

// fbFastUpstream answers a plain or a stream request at once.
func fbFastUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(fbChunk))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rv2GoodBody))
	}))
}

func fbConfig(slow, slow2, fast string) string {
	tiers := func(a, b string) string {
		return `"tiers":[{"name":"t0","minScore":0,"models":["` + a + `/m"]},{"name":"t1","minScore":300,"models":["` + b + `/m"]}]`
	}
	return `{"providers":{` + rv2ProviderJSON("fb-slow", slow) + `,` + rv2ProviderJSON("fb-slow2", slow2) + `,` + rv2ProviderJSON("fb-fast", fast) + `},` +
		`"groups":{"rv2g":{}},` +
		`"users":{"inline":[{"name":"rv2-user","group":"rv2g","apiKey":"` + rv2UserKey + `"}]},` +
		`"smartRouters":{` +
		`"` + fbOK + `":{"firstByteTimeout":"` + fbTimeout + `",` + tiers("fb-slow", "fb-fast") + `},` +
		`"` + fbDead + `":{"firstByteTimeout":"` + fbTimeout + `",` + tiers("fb-slow", "fb-slow2") + `}}}`
}

func fbPost(handler http.Handler, model string, stream bool) (*httptest.ResponseRecorder, time.Duration) {
	extra := ""
	if stream {
		extra = `,"stream":true`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]`+extra+`}`,
	))
	req.Header.Set("Authorization", "Bearer "+rv2UserKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	handler.ServeHTTP(rec, req)
	return rec, time.Since(start)
}

// exerciseRouterFirstByte checks that a slow first tier escalates to the next one for a plain request and for
// a stream whose headers arrive at once, and that a router with no timely candidate answers 504.
func exerciseRouterFirstByte(i *interp.Interpreter, pkgName string) error {
	slow, slow2, fast := fbSlowUpstream(), fbSlowUpstream(), fbFastUpstream()
	defer slow.Close()
	defer slow2.Close()
	defer fast.Close()

	handler, err := newRouterV2Handler(i, pkgName, fbConfig(slow.URL, slow2.URL, fast.URL))
	if err != nil {
		return err
	}
	for _, stream := range []bool{false, true} {
		rec, took := fbPost(handler, fbOK, stream)
		if rec.Code != http.StatusOK || rec.Header().Get("X-Llmgw-Router-Tier") != "t1" || took > fbBudget {
			return fmt.Errorf("router %q stream=%v with a slow t0: status=%d tier=%q took=%s, want 200 t1 within %s (body %s)",
				fbOK, stream, rec.Code, rec.Header().Get("X-Llmgw-Router-Tier"), took, fbBudget, rec.Body.String())
		}
	}

	rec, took := fbPost(handler, fbDead, false)
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), "did not start responding in time") || took > fbBudget {
		return fmt.Errorf("router %q with every model slow: status=%d took=%s body=%s, want 504 within %s", fbDead, rec.Code, took, rec.Body.String(), fbBudget)
	}
	fmt.Println("yaegi-check: a slow first tier escalated for a plain and a stream request, and a router with no timely model answered 504 under the interpreter")
	return nil
}
