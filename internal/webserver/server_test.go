package webserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"mattermost-locker/internal/webserver/command"
)

type fakeDispatcher struct {
	got *command.Request
	err error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req *command.Request) (*command.Response, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return command.Ephemeral("ok: " + req.Text), nil
}

func post(t *testing.T, s *Server, form url.Values, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthAndDispatch(t *testing.T) {
	d := &fakeDispatcher{}
	s := New(8080, "secret", d)

	form := url.Values{"token": {"secret"}, "text": {"lock lock1"}, "user_id": {"u1"}, "user_name": {"alice"}, "command": {"/lock-bot"}}
	rec := post(t, s, form, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var resp command.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ResponseType != "ephemeral" || resp.Text != "ok: lock lock1" {
		t.Errorf("resp %+v", resp)
	}
	if d.got.UserID != "u1" || d.got.UserName != "alice" || d.got.Command != "/lock-bot" {
		t.Errorf("request fields not mapped: %+v", d.got)
	}

	// Token via Authorization header also works.
	d.got = nil
	rec = post(t, s, url.Values{"text": {"status"}}, http.Header{"Authorization": {"Token secret"}})
	if rec.Code != 200 || d.got == nil {
		t.Errorf("header auth failed: %d", rec.Code)
	}
}

func TestRejectsBadTokens(t *testing.T) {
	d := &fakeDispatcher{}
	s := New(8080, "secret", d)

	for name, tc := range map[string]struct {
		form   url.Values
		header http.Header
	}{
		"wrong form token":   {url.Values{"token": {"nope"}}, nil},
		"no token":           {url.Values{"text": {"x"}}, nil},
		"wrong header token": {url.Values{}, http.Header{"Authorization": {"Bearer nope"}}},
	} {
		rec := post(t, s, tc.form, tc.header)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	if d.got != nil {
		t.Error("dispatcher must not run for unauthenticated requests")
	}
}

func TestDispatchErrorBecomesEphemeralMessage(t *testing.T) {
	s := New(8080, "secret", &fakeDispatcher{err: context.DeadlineExceeded})
	rec := post(t, s, url.Values{"token": {"secret"}}, nil)
	var resp command.Response
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp.ResponseType != "ephemeral" || !strings.Contains(resp.Text, "error occurred") {
		t.Errorf("%d %+v", rec.Code, resp)
	}
	if strings.Contains(resp.Text, "deadline") {
		t.Error("internal error details must not be shown to users")
	}
}

func TestOnlyPOST(t *testing.T) {
	s := New(8080, "secret", &fakeDispatcher{})
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/commands", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d", rec.Code)
	}
}

func TestStartFailsWhenPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	s := New(port, "t", &fakeDispatcher{})
	if err := s.Start(); err == nil {
		s.Shutdown(context.Background())
		t.Fatal("expected Start to fail when the port is already in use")
	}
}

func TestStartAndShutdown(t *testing.T) {
	s := New(0, "t", &fakeDispatcher{}) // port 0: any free port
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
