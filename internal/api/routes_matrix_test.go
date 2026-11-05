package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/users"
)

// The route matrix (day phase 7, cross-cutting checks; roles in Phase 4a):
// every pattern the real mux registers must be classified in routeRoles
// (roles.go, the role class) and listed below (a sample URL and whether it
// audits), and each class is checked as a matrix of credentials, roles and
// methods. A new route without a class panics at startup and fails
// TestRouteTableClassified.

type routeSpec struct {
	// class is the route's role class, kept here independently of
	// routeRoles (roles.go) so a changed class fails the test.
	class roleClass
	// target overrides the sample URL built from the pattern.
	target string
	// noAudit says why a state-changing route writes no audit record of
	// its own ("" means it must write one).
	noAudit string
}

// classOf is a route's role class as this table says.
func classOf(p string) roleClass { return routeTable[p].class }

// wantRoles are the roles each class admits (docs/SCOPE.md "Route → role
// class table"), independent of classRoles in roles.go.
var wantRoles = map[roleClass][]string{
	clsAny:     {"admin", "user", "viewer", "monitor", "chat"},
	clsAdmin:   {"admin"},
	clsWork:    {"admin", "user"},
	clsWatch:   {"admin", "user", "viewer"},
	clsMon:     {"admin", "viewer", "monitor"},
	clsChat:    {"admin", "user", "chat"},
	clsGateway: {"admin", "user"},
}

// ownerClass reports whether people (admin token, devices, sign-ins) may
// reach the class at all.
func ownerClass(c roleClass) bool {
	return c != clsPublic && c != clsNotFound && c != clsAgent
}

// routeTable classifies every registered pattern.
var routeTable = map[string]routeSpec{
	// Public.
	"GET /healthz":              {class: clsPublic},
	"GET /login":                {class: clsPublic},
	"GET /assets/":              {class: clsPublic, target: "/assets/app.css"},
	"GET /manifest.webmanifest": {class: clsPublic},
	"GET /sw.js":                {class: clsPublic},
	"GET /carino-clock.js":      {class: clsPublic},

	// UI, previews, gateway.
	"GET /{$}":                    {class: clsAny},
	"GET /preview/{id}":           {class: clsWatch},
	"GET /preview/{id}/{path...}": {class: clsWatch, target: "/preview/s1/index.html"},
	"/v1/":                        {class: clsGateway, target: "/v1/models", noAudit: "the gateway is recorded by usage"},

	// Core API.
	"GET /api/status":                            {class: clsAny},
	"GET /api/models":                            {class: clsWork},
	"POST /api/models/{rest...}":                 {class: clsAdmin, target: "/api/models/m1/load"},
	"GET /api/usage":                             {class: clsWork},
	"POST /api/usage/watch":                      {class: clsWork, noAudit: "the someone-is-looking switch changes nothing"},
	"GET /api/repos":                             {class: clsWork},
	"GET /api/repos/find":                        {class: clsAdmin},
	"GET /api/bench":                             {class: clsWork},
	"POST /api/bench":                            {class: clsAdmin},
	"GET /api/bench/{id}":                        {class: clsWork, target: "/api/bench/20261105-100000-qwen"},
	"GET /api/records":                           {class: clsAdmin},
	"GET /api/records/{day}/{file}/{offset}":     {class: clsAdmin, target: "/api/records/2026-11-05/1-2/0"},
	"DELETE /api/records":                        {class: clsAdmin, target: "/api/records?before=2026-01-01"},
	"GET /api/secrets":                           {class: clsAdmin},
	"PUT /api/secrets/{name}":                    {class: clsAdmin},
	"DELETE /api/secrets/{name}":                 {class: clsAdmin},
	"GET /api/search":                            {class: clsWatch},
	"POST /api/prune":                            {class: clsAdmin},
	"GET /api/sessions/{id}/transcript":          {class: clsWatch},
	"GET /api/batches":                           {class: clsWatch},
	"POST /api/batches":                          {class: clsWork},
	"GET /api/batches/{id}":                      {class: clsWatch},
	"POST /api/batches/{id}/{action}":            {class: clsWork, target: "/api/batches/b1/discard"},
	"GET /api/voice":                             {class: clsChat},
	"POST /api/voice/transcribe":                 {class: clsChat},
	"GET /api/sessions":                          {class: clsWatch},
	"POST /api/sessions":                         {class: clsWork},
	"GET /api/sessions/{id}":                     {class: clsWatch},
	"GET /api/sessions/{id}/diff":                {class: clsWatch},
	"GET /api/sessions/{id}/stream":              {class: clsWatch},
	"GET /api/sessions/{id}/archive":             {class: clsWatch},
	"GET /api/sessions/{id}/preview":             {class: clsWatch},
	"POST /api/sessions/{id}/preview/start":      {class: clsWork},
	"POST /api/sessions/{id}/preview/stop":       {class: clsWork},
	"GET /api/fs":                                {class: clsAdmin},
	"POST /api/sessions/{id}/{action}":           {class: clsWork, target: "/api/sessions/s1/stop"},
	"GET /api/events":                            {class: clsAny},
	"GET /api/monitor":                           {class: clsMon},
	"GET /api/monitor/processes":                 {class: clsMon},
	"GET /api/monitor/series":                    {class: clsMon},
	"GET /api/monitor/stream":                    {class: clsMon},
	"POST /api/monitor/processes/{pid}/{action}": {class: clsAdmin, target: "/api/monitor/processes/notapid/term"},
	"/api/": {class: clsAny, target: "/api/no-such-endpoint"},

	// Control center.
	"PATCH /api/sessions/{id}": {class: clsWork},
	"POST /api/sessions/bulk":  {class: clsWork},
	"GET /api/feed":            {class: clsWatch},
	"GET /api/feed/stream":     {class: clsWatch},
	"GET /api/wall/stream":     {class: clsWatch},

	// Agent scope.
	"POST /api/agent/spawn":              {class: clsAgent},
	"GET /api/agent/children":            {class: clsAgent},
	"GET /api/agent/models":              {class: clsAgent},
	"POST /api/agent/ask":                {class: clsAgent},
	"POST /api/agent/rate":               {class: clsAgent},
	"GET /api/agent/sessions/{id}/tail":  {class: clsAgent, target: "/api/agent/sessions/s2/tail"},
	"POST /api/agent/sessions/{id}/send": {class: clsAgent, target: "/api/agent/sessions/s2/send"},
	"GET /api/agent/sessions/{id}/wait":  {class: clsAgent, target: "/api/agent/sessions/s2/wait?timeout=0"},
	"POST /api/agent/sessions/{id}/stop": {class: clsAgent, target: "/api/agent/sessions/s2/stop"},
	"GET /api/agent/files":               {class: clsAgent},
	"GET /api/agent/files/raw":           {class: clsAgent, target: "/api/agent/files/raw?path=x"},
	"GET /api/agent/runs":                {class: clsAgent},
	"GET /api/agent/runs/{name}":         {class: clsAgent, target: "/api/agent/runs/x.cast"},
	"/api/agent/":                        {class: clsAgent, target: "/api/agent/no-such-endpoint"},

	// Files.
	"POST /api/sessions/{id}/files":       {class: clsWork},
	"GET /api/sessions/{id}/files":        {class: clsWatch},
	"GET /api/sessions/{id}/files/raw":    {class: clsWatch},
	"DELETE /api/sessions/{id}/files":     {class: clsWork, target: "/api/sessions/s1/files?path=x"},
	"POST /api/sessions/{id}/files/mkdir": {class: clsWork},
	"POST /api/uploads":                   {class: clsWork},
	"GET /api/uploads/{staging}":          {class: clsWork},
	"DELETE /api/uploads/{staging}":       {class: clsWork},

	// Safety.
	"GET /api/audit":           {class: clsAdmin},
	"GET /api/devices":         {class: clsAny},
	"POST /api/devices":        {class: clsAny},
	"DELETE /api/devices/{id}": {class: clsAny},
	"POST /api/token/rotate":   {class: clsAdmin},
	"GET /api/doctor":          {class: clsMon},

	// Configuration.
	"GET /api/config":            {class: clsAdmin},
	"GET /api/config/schema":     {class: clsAdmin},
	"PATCH /api/config":          {class: clsAdmin},
	"POST /api/config/restart":   {class: clsAdmin},
	"POST /api/config/init-done": {class: clsAdmin},

	// LLM chat: people only.
	"GET /api/chats":                       {class: clsChat},
	"POST /api/chats":                      {class: clsChat},
	"GET /api/chats/models":                {class: clsChat},
	"GET /api/chats/search":                {class: clsChat},
	"GET /api/chats/atrium":                {class: clsAdmin},
	"POST /api/chats/atrium/{id}/continue": {class: clsAdmin},
	"GET /api/chats/claude":                {class: clsAdmin},
	"POST /api/chats/claude/{id}/continue": {class: clsAdmin},
	"GET /api/chats/asks":                  {class: clsChat},
	"GET /api/chats/rules":                 {class: clsChat},
	"DELETE /api/chats/rules":              {class: clsChat},
	"GET /api/chats/{id}":                  {class: clsChat},
	"PATCH /api/chats/{id}":                {class: clsChat},
	"DELETE /api/chats/{id}":               {class: clsChat},
	"POST /api/chats/{id}/messages":        {class: clsChat},
	"GET /api/chats/{id}/stream":           {class: clsChat},
	"POST /api/chats/{id}/answer":          {class: clsChat},
	"POST /api/chats/{id}/stop":            {class: clsChat},
	"GET /api/chats/{id}/rules":            {class: clsChat},
	"DELETE /api/chats/{id}/rules":         {class: clsChat},
	"POST /api/quick":                      {class: clsChat},

	// Extras.
	"GET /api/inbox":                     {class: clsAny},
	"POST /api/inbox/read":               {class: clsAny},
	"DELETE /api/inbox/{id}":             {class: clsAny},
	"GET /api/sessions/{id}/runs":        {class: clsWatch},
	"GET /api/sessions/{id}/runs/{name}": {class: clsWatch, target: "/api/sessions/s1/runs/x.cast"},
	"GET /api/downloads":                 {class: clsAdmin},
	"POST /api/downloads":                {class: clsAdmin},
	"DELETE /api/downloads/{id}":         {class: clsAdmin},
	"GET /api/leaderboard":               {class: clsWatch},
	"GET /api/delegation":                {class: clsWatch},
	"DELETE /api/sessions/{id}":          {class: clsWork, target: "/api/sessions/s1"},
	"GET /api/sessions/{id}/delegation":  {class: clsWatch, target: "/api/sessions/s1/delegation"},
	"GET /api/finetune":                  {class: clsAdmin},
	"GET /api/finetune/export":           {class: clsAdmin},

	// Fine-tuning (finetune.go).
	"GET /api/finetune/examples":               {class: clsAdmin},
	"GET /api/finetune/examples/{id}":          {class: clsAdmin},
	"POST /api/finetune/examples/{id}/rate":    {class: clsAdmin},
	"POST /api/finetune/examples/{id}/exclude": {class: clsAdmin},
	"POST /api/finetune/train":                 {class: clsAdmin},
	"GET /api/finetune/runs":                   {class: clsAdmin},

	// Flow graph (read-only).
	"GET /api/sessions/{id}/flow": {class: clsWatch},
	"GET /api/chats/{id}/flow":    {class: clsChat},

	// Flows (flows.go): people only.
	"GET /api/flows":                     {class: clsWork},
	"POST /api/flows":                    {class: clsWork},
	"GET /api/flows/templates":           {class: clsWork},
	"POST /api/flows/validate":           {class: clsWork},
	"POST /api/flows/test":               {class: clsWork},
	"GET /api/flows/runs":                {class: clsWork},
	"GET /api/flows/runs/{run}":          {class: clsWork, target: "/api/flows/runs/fr-20261102-080000-abcdef"},
	"POST /api/flows/runs/{run}/cancel":  {class: clsWork, target: "/api/flows/runs/fr-20261102-080000-abcdef/cancel"},
	"POST /api/flows/runs/{run}/approve": {class: clsWork, target: "/api/flows/runs/fr-20261102-080000-abcdef/approve"},
	"GET /api/flows/runs/{run}/flow":     {class: clsWork, target: "/api/flows/runs/fr-20261102-080000-abcdef/flow"},
	"GET /api/flows/{id}":                {class: clsWork, target: "/api/flows/fl-sample"},
	"PUT /api/flows/{id}":                {class: clsWork, target: "/api/flows/fl-sample"},
	"DELETE /api/flows/{id}":             {class: clsWork, target: "/api/flows/fl-sample"},
	"POST /api/flows/{id}/run":           {class: clsWork, target: "/api/flows/fl-sample/run"},

	// Settings export and import.
	"GET /api/config/export":         {class: clsAdmin},
	"POST /api/config/import":        {class: clsAdmin},
	"POST /api/config/models/forget": {class: clsAdmin},

	// Users and roles (Phase 4a).
	"/":                             {class: clsNotFound, target: "/no-such-page"},
	"GET /api/auth":                 {class: clsPublic},
	"POST /api/login":               {class: clsPublic, noAudit: "recorded as auth.* events"},
	"POST /api/logout":              {class: clsAny},
	"GET /api/me":                   {class: clsAny},
	"POST /api/me/password":         {class: clsAny},
	"GET /api/me/logins":            {class: clsAny},
	"DELETE /api/me/logins/{id}":    {class: clsAny},
	"POST /api/me/apikey":           {class: clsWork},
	"DELETE /api/me/apikey":         {class: clsWork},
	"GET /api/users":                {class: clsAdmin},
	"POST /api/users":               {class: clsAdmin},
	"GET /api/users/{id}":           {class: clsAdmin},
	"PATCH /api/users/{id}":         {class: clsAdmin},
	"POST /api/users/{id}/password": {class: clsAdmin},
	"DELETE /api/users/{id}":        {class: clsAdmin},
	"DELETE /api/users/{id}/apikey": {class: clsAdmin},

	// Saved permissions.
	"GET /api/permits":         {class: clsWork},
	"POST /api/permits":        {class: clsWork},
	"DELETE /api/permits/{id}": {class: clsWork},
}

// splitPattern returns the pattern's methods (all common ones when it
// names none) and its path.
func splitPattern(p string) ([]string, string) {
	if m, rest, ok := strings.Cut(p, " "); ok {
		return []string{m}, rest
	}
	return []string{"GET", "POST", "PUT", "PATCH", "DELETE"}, p
}

// sampleTarget is a concrete URL the pattern serves: wildcards become
// "s1", {$} disappears.
func sampleTarget(p string, spec routeSpec) string {
	if spec.target != "" {
		return spec.target
	}
	_, path := splitPattern(p)
	var b strings.Builder
	for {
		i := strings.IndexByte(path, '{')
		if i < 0 {
			b.WriteString(path)
			return b.String()
		}
		j := strings.IndexByte(path[i:], '}')
		b.WriteString(path[:i])
		if path[i:i+j+1] != "{$}" {
			b.WriteString("s1")
		}
		path = path[i+j+1:]
	}
}

func isWrite(m string) bool { return !safeMethod(m) }

// refused: 401/403, or the UI's redirect to /login.
func refused(code int, target string) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden ||
		(code == http.StatusFound && target == "/")
}

// matrixEnv is a safety env (audit log, devices, config file in temp
// folders) plus a probe between auth and the audit layer that records
// whether a request was admitted. The chain is the one handler() builds.
type matrixEnv struct {
	*safetyEnv
	t       *testing.T
	reached bool
	chain   http.Handler
}

func newMatrixEnv(t *testing.T) *matrixEnv {
	t.Helper()
	se := newSafetyEnv(t, nil)
	m := &matrixEnv{safetyEnv: se, t: t}
	probe := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.reached = true
			next.ServeHTTP(w, r)
		})
	}
	s := se.srv
	m.chain = s.withHeaders(s.withHost(s.withAuth(probe(s.withAudit(s.mux)))))
	return m
}

func (m *matrixEnv) send(method, target string, opts ...reqOpt) (int, bool, string) {
	m.reached = false
	var body io.Reader
	if isWrite(method) {
		body = strings.NewReader("{}")
	}
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "192.0.2.10:5555"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	if method == http.MethodGet && isStreamPath(r.URL.Path) {
		// An admitted stream would run until the client leaves.
		ctx, cancel := context.WithTimeout(r.Context(), 150*time.Millisecond)
		defer cancel()
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	m.chain.ServeHTTP(w, r)
	if !m.reached {
		// A refusal has no side effects: the real handler() chain must
		// refuse the same way, so the probed copy cannot drift from it.
		r2 := r.Clone(r.Context())
		if body != nil {
			r2.Body = io.NopCloser(strings.NewReader("{}"))
		}
		w2 := httptest.NewRecorder()
		m.h.ServeHTTP(w2, r2)
		if w2.Code != w.Code {
			m.t.Errorf("%s %s: probed chain answered %d, handler() %d", method, target, w.Code, w2.Code)
		}
	}
	return w.Code, m.reached, strings.TrimSpace(w.Body.String())
}

// TestRouteTableClassified: the mux, routeRoles and the table list the
// same patterns.
func TestRouteTableClassified(t *testing.T) {
	env := newEnv(t, nil)
	routes := env.srv.routes
	if len(routes) < 50 {
		t.Fatalf("only %d routes recorded: is routeMux still in use?", len(routes))
	}
	for _, p := range routes {
		if _, ok := routeTable[p]; !ok {
			t.Errorf("route %q is not listed in routeTable (routes_matrix_test.go)", p)
		}
		if c, ok := routeRoles[p]; !ok {
			t.Errorf("route %q has no role class in routeRoles (roles.go)", p)
		} else if c != routeTable[p].class {
			t.Errorf("route %q: routeRoles says %s, the test table %s", p, c, routeTable[p].class)
		}
		// The recorded pattern is what the mux really serves at its sample URL.
		methods, _ := splitPattern(p)
		r := httptest.NewRequest(methods[0], sampleTarget(p, routeTable[p]), nil)
		if _, got := env.srv.mux.Handler(r); got != p {
			t.Errorf("sample URL for %q is served by %q", p, got)
		}
	}
	for p := range routeTable {
		if !slices.Contains(routes, p) {
			t.Errorf("routeTable lists %q, which the mux does not register", p)
		}
	}
	for p := range routeRoles {
		if !slices.Contains(routes, p) {
			t.Errorf("routeRoles lists %q, which the mux does not register", p)
		}
	}
}

// TestRouteMatrix checks auth, CSRF and the agent scope for every route,
// before any user account exists (everyone is admin, as before Phase 4a).
func TestRouteMatrix(t *testing.T) {
	env := newMatrixEnv(t)
	agentTok := env.srv.cfg.SessionAgentToken("s1")
	sharedTok := env.srv.cfg.AgentToken()
	deviceTok, _ := env.pair(t, "phone")
	asAgent := []reqOpt{withBearer(agentTok), withHeader(sessionHeader, "s1")}
	for _, p := range env.srv.routes {
		spec, ok := routeTable[p]
		if !ok {
			continue // TestRouteTableClassified reports it
		}
		class := classOf(p)
		public := class == clsPublic || class == clsNotFound
		target := sampleTarget(p, spec)
		methods, _ := splitPattern(p)
		for _, m := range methods {
			name := m + " " + target + " (" + p + ")"

			// 1. No credential: 401 (or the UI's redirect to /login).
			code, reached, body := env.send(m, target)
			switch {
			case public:
				if !reached {
					t.Errorf("%s: public route refused without a credential: %d %s", name, code, body)
				}
			case reached:
				t.Errorf("%s: admitted without a credential (%d)", name, code)
			case code != http.StatusUnauthorized && !(code == http.StatusFound && target == "/"):
				t.Errorf("%s: no credential got %d, want 401 (or 302 /login for the UI)", name, code)
			}

			// 2. Cookie without the CSRF header on a write: 403. With it:
			// admitted, for owner routes.
			if isWrite(m) && !public {
				code, reached, body = env.send(m, target, withCookie(testToken))
				if reached || code != http.StatusForbidden {
					t.Errorf("%s: cookie write without %s got %d (admitted %v) %s, want 403", name, csrfHeader, code, reached, body)
				}
			}
			if ownerClass(class) {
				// Rotation and restart would change the env under the rest of the matrix.
				if isWrite(m) && p != "POST /api/token/rotate" && p != "POST /api/config/restart" {
					if code, reached, body = env.send(m, target, withCookie(testToken), withHeader(csrfHeader, "1")); !reached || isRoleRefusal(code, body) {
						t.Errorf("%s: cookie write with %s was not admitted (%d %s)", name, csrfHeader, code, body)
					}
					if code, reached, body = env.send(m, target, withBearer(deviceTok)); !reached || isRoleRefusal(code, body) {
						t.Errorf("%s: paired device write was not admitted (%d %s)", name, code, body)
					}
				}
			}

			// 3. The session agent token reaches only the agent scope and
			// /v1; the shared agent token only /v1.
			if public {
				continue
			}
			code, reached, body = env.send(m, target, asAgent...)
			wantAgent := class == clsAgent || class == clsGateway
			if reached != wantAgent {
				t.Errorf("%s: session agent token admitted=%v, want %v (%d %s)", name, reached, wantAgent, code, body)
			}
			if !reached && !refused(code, target) {
				t.Errorf("%s: session agent token refused with %d, want 401/403", name, code)
			}
			code, reached, body = env.send(m, target, withBearer(sharedTok))
			if wantShared := class == clsGateway; reached != wantShared {
				t.Errorf("%s: shared agent token admitted=%v, want %v (%d %s)", name, reached, wantShared, code, body)
			}
			if !reached && !refused(code, target) {
				t.Errorf("%s: shared agent token refused with %d, want 401/403", name, code)
			}
			// The owner's credentials never act as an agent.
			if class == clsAgent {
				for who, tok := range map[string]string{"admin": testToken, "device": deviceTok} {
					if _, reached, _ = env.send(m, target, withBearer(tok), withHeader(sessionHeader, "s1")); reached {
						t.Errorf("%s: %s token admitted to the agent scope", name, who)
					}
				}
			}
		}
	}
}

// isRoleRefusal reports whether a reply is the class gate's 403.
func isRoleRefusal(code int, body string) bool {
	return code == http.StatusForbidden && strings.Contains(body, `"code":"role"`)
}

// roleCreds are one account per role with a login cookie and a paired
// device, a personal API key and an MCP client for the work roles.
type roleCreds struct {
	user   users.User
	cookie string
	device string
	mcp    string
	key    string
}

// withRoles adds one account per role to a matrix env (the first is the
// admin) and returns their credentials.
func withRoles(t *testing.T, env *safetyEnv) map[string]roleCreds {
	t.Helper()
	t.Cleanup(users.SetCostForTest(1000))
	st, err := users.Open(env.srv.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	env.srv.users = st
	out := map[string]roleCreds{}
	for _, role := range users.Roles {
		u, err := st.Add("u-"+role, "", role, "a long password")
		if err != nil {
			t.Fatal(err)
		}
		c := roleCreds{user: u}
		if c.cookie, _, err = st.Logins.Create(u.ID, "192.0.2.10", "test"); err != nil {
			t.Fatal(err)
		}
		if c.device, _, err = env.devices.PairScoped("dev-"+role, "", u.ID); err != nil {
			t.Fatal(err)
		}
		if c.mcp, _, err = env.devices.PairScoped("mcp-"+role, devices.ScopeMCP, u.ID); err != nil {
			t.Fatal(err)
		}
		if c.key, err = st.NewAPIKey(u.ID); err != nil {
			t.Fatal(err)
		}
		out[role] = c
	}
	return out
}

// TestRouteMatrixRoles runs every pattern and method for each of the five
// roles (login cookie and paired device), the admin token, the personal
// API key and the MCP client: admitted or 403 code "role" exactly per the
// route's class.
func TestRouteMatrixRoles(t *testing.T) {
	env := newMatrixEnv(t)
	creds := withRoles(t, env.safetyEnv)
	skip := map[string]bool{"POST /api/token/rotate": true, "POST /api/config/restart": true,
		"POST /api/logout": true, "DELETE /api/users/{id}": true}
	for _, p := range env.srv.routes {
		spec, ok := routeTable[p]
		if !ok || skip[p] {
			continue
		}
		class := classOf(p)
		target := sampleTarget(p, spec)
		methods, _ := splitPattern(p)
		for _, m := range methods {
			name := m + " " + target + " (" + p + ")"
			for _, role := range users.Roles {
				c := creds[role]
				for kind, opts := range map[string][]reqOpt{
					"login":  {withCookie(c.cookie), withHeader(csrfHeader, "1")},
					"device": {withBearer(c.device)},
				} {
					want := slices.Contains(wantRoles[class], role)
					if want && m == http.MethodGet && isStreamPath(strings.SplitN(target, "?", 2)[0]) {
						continue // admitted streams are covered by their own tests
					}
					code, reached, body := env.send(m, target, opts...)
					switch {
					case class == clsPublic || class == clsNotFound:
						if !reached {
							t.Errorf("%s as %s %s: public route refused (%d)", name, role, kind, code)
						}
					case class == clsAgent:
						if reached {
							t.Errorf("%s as %s %s: a person reached the agent scope", name, role, kind)
						}
					case want && isRoleRefusal(code, body):
						t.Errorf("%s as %s %s: refused by role, want admitted (%s)", name, role, kind, body)
					case !want && !isRoleRefusal(code, body):
						t.Errorf("%s as %s %s: got %d %s, want 403 code role", name, role, kind, code, body)
					}
				}
				// The personal API key opens /v1 only, for admin and user.
				code, reached, body := env.send(m, target, withBearer(c.key))
				switch {
				case class == clsGateway:
					want := role == users.RoleAdmin || role == users.RoleUser
					if want != (reached && !isRoleRefusal(code, body)) {
						t.Errorf("%s with %s's API key: %d %s (want admitted %v)", name, role, code, body, want)
					}
				case class == clsPublic || class == clsNotFound:
				case reached:
					t.Errorf("%s with %s's API key: admitted outside /v1 (%d)", name, role, code)
				}
				// The MCP client acts only in the agent scope, for admin and user.
				code, reached, body = env.send(m, target, withBearer(c.mcp))
				switch {
				case class == clsAgent:
					want := role == users.RoleAdmin || role == users.RoleUser
					if want != (reached && !isRoleRefusal(code, body)) {
						t.Errorf("%s with %s's MCP client: %d %s (want admitted %v)", name, role, code, body, want)
					}
				case class == clsPublic || class == clsNotFound:
				case reached && !isRoleRefusal(code, body):
					t.Errorf("%s with %s's MCP client: admitted outside the agent scope (%d)", name, role, code)
				}
			}
			// The admin token is admin.
			if ownerClass(class) && class != clsGateway && !(m == http.MethodGet && isStreamPath(strings.SplitN(target, "?", 2)[0])) {
				code, _, body := env.send(m, target, withBearer(testToken))
				if isRoleRefusal(code, body) {
					t.Errorf("%s: the admin token was refused by role", name)
				}
			}
		}
	}
}

// TestRouteMatrixMustChange: a sign-in that must change its password gets
// 403 must_change everywhere but its allow-list; its devices too.
func TestRouteMatrixMustChange(t *testing.T) {
	env := newMatrixEnv(t)
	creds := withRoles(t, env.safetyEnv)
	u := creds[users.RoleUser].user
	if _, err := env.srv.users.SetPassword(u.ID, "a fresh password", true); err != nil {
		t.Fatal(err)
	}
	c := creds[users.RoleUser]
	for _, p := range env.srv.routes {
		spec := routeTable[p]
		class := classOf(p)
		if !ownerClass(class) || class == clsGateway || !slices.Contains(wantRoles[class], users.RoleUser) {
			continue
		}
		if p == "GET /api/events" || p == "POST /api/logout" {
			continue // a stream of pings; logout ends the login
		}
		methods, _ := splitPattern(p)
		target := sampleTarget(p, spec)
		for _, m := range methods {
			if m == http.MethodGet && isStreamPath(strings.SplitN(target, "?", 2)[0]) {
				continue
			}
			for kind, opts := range map[string][]reqOpt{
				"login":  {withCookie(c.cookie), withHeader(csrfHeader, "1")},
				"device": {withBearer(c.device)},
			} {
				code, _, body := env.send(m, target, opts...)
				blocked := code == http.StatusForbidden && strings.Contains(body, `"code":"must_change"`)
				if blocked == mustChangeAllowed[p] {
					t.Errorf("%s %s (%s): %d %s; allowed=%v", m, target, kind, code, body, mustChangeAllowed[p])
				}
			}
		}
	}
}

// TestRouteMatrixAudit: every state-changing request leaves an audit
// record naming its route pattern, whatever the handler answered. Each
// route runs in a fresh env (token rotation, config saves).
func TestRouteMatrixAudit(t *testing.T) {
	routes := newEnv(t, nil).srv.routes
	for _, p := range routes {
		spec, ok := routeTable[p]
		class := classOf(p)
		if !ok || class == clsPublic || class == clsNotFound {
			continue
		}
		methods, _ := splitPattern(p)
		for _, m := range methods {
			if !isWrite(m) {
				continue
			}
			target := sampleTarget(p, spec)
			t.Run(m+" "+p, func(t *testing.T) {
				env := newMatrixEnv(t)
				opts := []reqOpt{withBearer(testToken)}
				actor := audit.Owner("admin")
				if class == clsAgent {
					opts = []reqOpt{withBearer(env.srv.cfg.SessionAgentToken("s1")), withHeader(sessionHeader, "s1")}
					actor = audit.Agent("s1")
				}
				code, reached, _ := env.send(m, target, opts...)
				if !reached {
					t.Fatalf("not admitted (%d)", code)
				}
				var got []audit.Record
				for _, rec := range env.records(t) {
					if rec.Route == p {
						got = append(got, rec)
					}
				}
				if spec.noAudit != "" {
					if len(got) != 0 {
						t.Fatalf("exempt route (%s) wrote %d records", spec.noAudit, len(got))
					}
					return
				}
				if len(got) != 1 {
					t.Fatalf("%d audit records for route %q (status %d), want 1", len(got), p, code)
				}
				if got[0].Actor != actor || got[0].Method != m || got[0].Status != code || got[0].User != "" {
					t.Fatalf("record %+v, want actor %s method %s status %d, no user", got[0], actor, m, code)
				}
			})
		}
	}
}

// TestRouteMatrixAuditUser: a signed-in user's writes carry their name.
func TestRouteMatrixAuditUser(t *testing.T) {
	env := newMatrixEnv(t)
	creds := withRoles(t, env.safetyEnv)
	c := creds[users.RoleUser]
	code, reached, _ := env.send("POST", "/api/inbox/read", withCookie(c.cookie), withHeader(csrfHeader, "1"))
	if !reached {
		t.Fatalf("not admitted (%d)", code)
	}
	var found bool
	for _, rec := range env.records(t) {
		if rec.Route == "POST /api/inbox/read" {
			found = true
			if rec.User != c.user.Name || rec.Actor != audit.User(c.user.Name) {
				t.Fatalf("record %+v", rec)
			}
		}
	}
	if !found {
		t.Fatal("no record")
	}
	// A device of the user: actor owner:<device>, user the name.
	code, _, _ = env.send("POST", "/api/inbox/read", withBearer(c.device))
	recs := env.records(t)
	last := recs[0]
	for _, r := range recs {
		if r.Time.After(last.Time) {
			last = r
		}
	}
	_ = code
	if last.User != c.user.Name || !strings.HasPrefix(last.Actor, "owner:dev-") {
		t.Fatalf("device record %+v", last)
	}
}
