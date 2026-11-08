package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestFishingRuleSwitchAPIAndRestart(t *testing.T) {
	for _, key := range []string{"fish_ecology_enabled", "fish_bait_rules_enabled", "fish_distribution_enabled", "fish_weather_enabled", "happy_hour_enabled", "grade_ratio_enabled"} {
		t.Run(key, func(t *testing.T) { testFishingSwitchAPIAndRestart(t, key) })
	}
}
func testFishingSwitchAPIAndRestart(t *testing.T, key string) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.EnsureAdmin(ctx, "admin", "rules-test"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(config.Admin{JWTSecret: "rules-test"}, db, logging.Discard(), WebFS())
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	var login struct {
		Token string `json:"token"`
	}
	w := call("POST", "/api/login", "", `{"username":"admin","password":"rules-test"}`)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &login) != nil || login.Token == "" {
		t.Fatalf("login=%s", w.Body)
	}
	if _, exists, err := db.TunableGet(ctx, key); err != nil && !errors.Is(err, sql.ErrNoRows) || (exists && key != "happy_hour_enabled") {
		t.Fatalf("default should be unset: %v", err)
	}
	if got := call("POST", "/api/tunables", "", `{"key":"`+key+`","value":"1"}`).Code; got != 401 {
		t.Fatalf("unauthenticated=%d", got)
	}
	for _, v := range []string{"2", "-1", "true", "NaN", ""} {
		if got := call("POST", "/api/tunables", login.Token, `{"key":"`+key+`","value":"`+v+`"}`).Code; got != 400 {
			t.Fatalf("invalid %q=%d", v, got)
		}
	}
	for _, v := range []string{"1", "0", "1"} {
		w = call("POST", "/api/tunables", login.Token, `{"key":"`+key+`","value":"`+v+`"}`)
		if w.Code != 200 {
			t.Fatalf("save=%d %s", w.Code, w.Body)
		}
		assertQueueContains(t, db, "reload_tunables")
		if got, ok, err := db.TunableGet(ctx, key); err != nil || !ok || got != v {
			t.Fatalf("saved=%q %v", got, err)
		}
	}
	var audits int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='tunable' AND target=?`, key).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audit=%d %v", audits, err)
	}
	path := db.Path
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for range 2 {
		if err := reopened.Migrate(); err != nil {
			t.Fatal(err)
		}
	}
	if value, ok, err := reopened.TunableGet(ctx, key); err != nil || !ok || value != "1" {
		t.Fatalf("restart lost switch: %q %v", value, err)
	}
}
