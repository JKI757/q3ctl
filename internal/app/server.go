// Package app contains q3ctl's HTTP API and supervised runtime.
package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"q3ctl/internal/config"
	"q3ctl/internal/rcon"
	"q3ctl/internal/store"
	"q3ctl/pkg/q3"
)

type BotPolicy struct {
	Enabled          bool     `json:"enabled"`
	HumansSingleTeam bool     `json:"humans_single_team"`
	HumanTeam        string   `json:"human_team"`
	BotsPerTeam      int      `json:"bots_per_team"`
	FriendlyBots     []string `json:"friendly_bots"`
	OpponentBots     []string `json:"opponent_bots"`
	BaseSkill        int      `json:"base_skill"`
	Adaptive         bool     `json:"adaptive"`
	MinSkill         int      `json:"min_skill"`
	MaxSkill         int      `json:"max_skill"`
}

type Player struct {
	ID      int    `json:"id"`
	Score   int    `json:"score"`
	Ping    int    `json:"ping"`
	Name    string `json:"name"`
	RawName string `json:"raw_name"`
	Address string `json:"address"`
	Bot     bool   `json:"bot"`
	Team    string `json:"team,omitempty"`
}

type BotCounts struct {
	TargetPerTeam int  `json:"target_per_team"`
	Red           int  `json:"red"`
	Blue          int  `json:"blue"`
	Total         int  `json:"total"`
	TeamsKnown    bool `json:"teams_known"`
}
type Status struct {
	Map          string   `json:"map"`
	GameType     int      `json:"gametype"`
	TimeLimit    int      `json:"timelimit"`
	FragLimit    int      `json:"fraglimit"`
	CaptureLimit int      `json:"capturelimit"`
	MaxClients   int      `json:"max_clients"`
	Players      []Player `json:"players"`
	Raw          string   `json:"-"`
}
type HostStats struct {
	Load1           float64 `json:"load_1"`
	MemoryTotal     uint64  `json:"memory_total_bytes"`
	MemoryAvailable uint64  `json:"memory_available_bytes"`
	UptimeSeconds   uint64  `json:"uptime_seconds"`
}
type Persisted struct {
	Policy   BotPolicy   `json:"policy"`
	Rotation q3.Rotation `json:"rotation"`
}
type Audit struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
	Result string    `json:"result"`
}
type server struct {
	cfg          config.Config
	mu           sync.RWMutex
	state        Persisted
	subscribers  map[chan Audit]struct{}
	csrfToken    string
	mapCatalog   []q3.MapInfo
	mapCatalogAt time.Time
}

func defaults() Persisted {
	return Persisted{Policy: BotPolicy{true, true, "red", 4, []string{"Sarge", "Major", "Grunt", "Crash"}, []string{"Hunter", "Xaero", "Bitterman", "TankJr"}, 3, true, 1, 5}, Rotation: q3.Rotation{{Map: "q3ctf1", GameType: 4, TimeLimit: 20, FragLimit: 0, CaptureLimit: 8}, {Map: "q3ctf2", GameType: 4, TimeLimit: 20, FragLimit: 0, CaptureLimit: 8}, {Map: "q3dm6", GameType: 3, TimeLimit: 15, FragLimit: 40, CaptureLimit: 0}, {Map: "q3dm17", GameType: 3, TimeLimit: 15, FragLimit: 40, CaptureLimit: 0}}}
}
func New(cfg config.Config) *server {
	s := &server{cfg: cfg, state: defaults(), subscribers: map[chan Audit]struct{}{}, csrfToken: newCSRFToken()}
	s.loadState()
	return s
}

func (s *server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/api/v1/status", s.status)
	mux.HandleFunc("/api/v1/players", s.players)
	mux.HandleFunc("/api/v1/bots/policy", s.policy)
	mux.HandleFunc("/api/v1/bots/reconcile", s.reconcile)
	mux.HandleFunc("/api/v1/maps", s.maps)
	mux.HandleFunc("/api/v1/maps/load", s.loadMap)
	mux.HandleFunc("/api/v1/maps/restart", s.restartMap)
	mux.HandleFunc("/api/v1/rotation", s.rotation)
	mux.HandleFunc("/api/v1/rotation/apply", s.applyRotation)
	mux.HandleFunc("/api/v1/announce", s.announce)
	mux.HandleFunc("/api/v1/players/kick", s.kick)
	mux.HandleFunc("/api/v1/audit", s.audit)
	mux.HandleFunc("/api/v1/logs/stream", s.stream)
	mux.HandleFunc("/", s.dashboard)
	return s.auth(mux)
}

// Run owns the HTTP server lifecycle. Its serving goroutine always terminates
// before Run returns, whether the listener fails or the parent context ends.
func (s *server) Run(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       70 * time.Second,
	}
	errs := make(chan error, 1)
	go func() { errs <- httpServer.ListenAndServe() }()
	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errs
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
func newCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.AdminUser)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.AdminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="q3ctl"`)
			http.Error(w, "authentication required", 401)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			origin := r.Header.Get("Origin")
			host := "https://" + r.Host
			if origin != host || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrfToken)) != 1 {
				http.Error(w, "csrf validation failed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func out(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	out(w, map[string]any{"ok": true, "time": time.Now().UTC()})
}
func (s *server) rcon(command string) (string, error) {
	return s.rconFor(command, 3*time.Second)
}

func (s *server) rconFor(command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return (rcon.Client{Address: s.cfg.RCONAddr, Password: s.cfg.RCONPassword}).Execute(ctx, command)
}
func parseStatus(raw string) Status {
	st := Status{Raw: raw}
	lines := strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n")
	if len(lines) > 0 {
		kv := strings.Split(strings.TrimPrefix(lines[0], "\\"), "\\")
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		st.Map = m["mapname"]
		st.GameType, _ = strconv.Atoi(m["g_gametype"])
		st.TimeLimit, _ = strconv.Atoi(m["timelimit"])
		st.FragLimit, _ = strconv.Atoi(m["fraglimit"])
		st.CaptureLimit, _ = strconv.Atoi(m["capturelimit"])
		st.MaxClients, _ = strconv.Atoi(m["sv_maxclients"])
	}
	re := regexp.MustCompile(`^\s*(\d+)\s+(-?\d+)\s+(\d+)\s+(.+?)\s{2,}(.+)$`)
	for _, line := range lines {
		m := re.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		id, _ := strconv.Atoi(m[1])
		score, _ := strconv.Atoi(m[2])
		ping, _ := strconv.Atoi(m[3])
		rawName := strings.TrimSpace(m[4])
		name := stripQ3Colors(rawName)
		addr := strings.TrimSpace(m[5])
		st.Players = append(st.Players, Player{ID: id, Score: score, Ping: ping, Name: name, RawName: rawName, Address: addr, Bot: strings.Contains(strings.ToLower(addr), "bot")})
	}
	return st
}

// stripQ3Colors removes the two-byte ^<code> sequences that Q3 uses to color
// player names. The raw name is retained in the API for diagnostics.
func stripQ3Colors(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '^' && i+1 < len(s) {
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
func teamNameFromUserInfo(raw string) string {
	separator := string('\\')
	start := strings.Index(raw, separator)
	if start < 0 {
		return ""
	}
	fields := strings.Split(raw[start+1:], separator)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "t" {
			continue
		}
		switch fields[i+1] {
		case "1":
			return "red"
		case "2":
			return "blue"
		case "3":
			return "spectator"
		}
	}
	return ""
}

func botCounts(players []Player, target int) BotCounts {
	counts := BotCounts{TargetPerTeam: target}
	for _, player := range players {
		if !player.Bot {
			continue
		}
		counts.Total++
		switch player.Team {
		case "red":
			counts.Red++
		case "blue":
			counts.Blue++
		}
	}
	return counts
}

func hasExpectedBotNames(players []Player, names []string) bool {
	found := make(map[string]bool, len(names))
	for _, player := range players {
		if player.Bot {
			found[strings.ToLower(player.Name)] = true
		}
	}
	for _, name := range names {
		if !found[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

// waitForBotRoster uses the engine's status-address bot marker. Stock
// ioquake3 does not expose server-side team membership through dumpuser, so
// that client userinfo must never decide whether a destructive rebuild ran.
func (s *server) waitForBotRoster(want int, names []string, timeout time.Duration) (Status, error) {
	deadline := time.Now().Add(timeout)
	var last Status
	var lastErr error
	for time.Now().Before(deadline) {
		st, err := s.live()
		if err == nil {
			last = st
			if botCounts(st.Players, 0).Total == want && (want == 0 || hasExpectedBotNames(st.Players, names)) {
				return st, nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil && len(last.Players) == 0 {
		return Status{}, lastErr
	}
	return last, fmt.Errorf("observed %d bots, wanted %d", botCounts(last.Players, 0).Total, want)
}

func (s *server) populatePlayerTeams(st *Status) {
	if st.GameType != q3.GameTypeTDM && st.GameType != q3.GameTypeCTF {
		return
	}
	var wg sync.WaitGroup
	for index := range st.Players {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Team data is optional dashboard enrichment. A missing dumpuser reply
			// must never turn a status refresh into N sequential RCON timeouts.
			raw, err := s.rconFor(fmt.Sprintf("dumpuser %d", st.Players[index].ID), 700*time.Millisecond)
			if err == nil {
				st.Players[index].Team = teamNameFromUserInfo(raw)
			}
		}()
	}
	wg.Wait()
}

// verifiedBotCounts resolves team membership serially for the small bot subset
// immediately after a rebuild. Concurrent dumpuser calls are intentionally
// used for the dashboard, where team enrichment is best-effort; ioquake3 can
// however drop/reorder simultaneous RCON print replies. Do not use that
// best-effort result to reject an otherwise successful roster rebuild.
func (s *server) verifiedBotCounts(st *Status, target int) BotCounts {
	for index := range st.Players {
		if !st.Players[index].Bot {
			continue
		}
		raw, err := s.rconFor(fmt.Sprintf("dumpuser %d", st.Players[index].ID), 2*time.Second)
		if err == nil {
			st.Players[index].Team = teamNameFromUserInfo(raw)
		}
	}
	return botCounts(st.Players, target)
}

func parseInfoString(raw string) map[string]string {
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
		if !strings.HasPrefix(line, "\\") {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, "\\"), "\\")
		values := map[string]string{}
		for i := 0; i+1 < len(fields); i += 2 {
			values[fields[i]] = fields[i+1]
		}
		return values
	}
	return nil
}

func (s *server) live() (Status, error) {
	raw, err := s.rcon("status")
	if err != nil {
		return Status{}, err
	}
	st := parseStatus(raw)

	// Quake's authenticated `status` response is retained for the player table.
	// Its cvar representation varies across ioquake3 builds, so use the stable
	// standard getstatus info string for the running map and game mode.
	serverInfo, err := s.getStatusInfo()
	if err != nil {
		return Status{}, err
	}
	values := parseInfoString(serverInfo)
	if values == nil || values["mapname"] == "" {
		return Status{}, errors.New("Quake getstatus reply was not understood")
	}
	gameType, err := strconv.Atoi(values["g_gametype"])
	if err != nil {
		return Status{}, errors.New("Quake getstatus g_gametype was not understood")
	}
	st.Map = values["mapname"]
	st.GameType = gameType
	return st, nil
}

// getStatusInfo tolerates one transient UDP loss. A single localhost UDP
// timeout is not evidence that Quake changed mode or stopped serving; retrying
// is bounded and remains below ioquake3's getstatus rate limit.
func (s *server) getStatusInfo() (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		raw, err := (rcon.Client{Address: s.cfg.RCONAddr}).GetStatus(ctx)
		cancel()
		if err == nil {
			return raw, nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return "", lastErr
}
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	x, e := s.live()
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	s.mu.RLock()
	p := s.state
	s.mu.RUnlock()
	out(w, map[string]any{"server": x, "policy": p.Policy, "bot_counts": botCounts(x.Players, p.Policy.BotsPerTeam), "rotation": p.Rotation, "host": hostStats()})
}

// hostStats is best-effort: an unavailable /proc must never hide game status.
func hostStats() HostStats {
	stats := HostStats{}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		_, _ = fmt.Sscanf(string(data), "%f", &stats.Load1)
	}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		var uptime float64
		if _, scanErr := fmt.Sscanf(string(data), "%f", &uptime); scanErr == nil {
			stats.UptimeSeconds = uint64(uptime)
		}
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			var key string
			var kib uint64
			if _, scanErr := fmt.Sscanf(line, "%s %d kB", &key, &kib); scanErr != nil {
				continue
			}
			switch strings.TrimSuffix(key, ":") {
			case "MemTotal":
				stats.MemoryTotal = kib * 1024
			case "MemAvailable":
				stats.MemoryAvailable = kib * 1024
			}
		}
	}
	return stats
}
func (s *server) players(w http.ResponseWriter, r *http.Request) {
	x, e := s.live()
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	out(w, x.Players)
}
func (s *server) policy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out(w, s.state.Policy)
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(405)
		return
	}
	var p BotPolicy
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&p) != nil || validatePolicy(p) != nil {
		http.Error(w, "invalid bot policy", 400)
		return
	}
	counts, err := s.rebuildBots(p)
	if err != nil {
		s.record("bot_policy", "not saved because bot rebuild failed", err.Error())
		http.Error(w, "bot policy was not saved: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	s.state.Policy = p
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "bots were applied but policy could not be persisted", http.StatusInternalServerError)
		return
	}
	s.record("bot_policy", fmt.Sprintf("target=%d per team actual=%d red/%d blue", counts.TargetPerTeam, counts.Red, counts.Blue), "applied")
	out(w, map[string]any{"policy": p, "bot_counts": counts})
}
func validatePolicy(p BotPolicy) error {
	if p.HumanTeam != "red" && p.HumanTeam != "blue" {
		return errors.New("team")
	}
	if p.BotsPerTeam < 0 || p.BotsPerTeam > len(p.FriendlyBots) || p.BotsPerTeam > len(p.OpponentBots) {
		return errors.New("count")
	}
	if p.MinSkill < 1 || p.MaxSkill > 5 || p.BaseSkill < p.MinSkill || p.BaseSkill > p.MaxSkill {
		return errors.New("skill")
	}
	return nil
}
func (s *server) rebuildBots(p BotPolicy) (BotCounts, error) {
	current, err := s.live()
	if err != nil {
		return BotCounts{}, fmt.Errorf("could not read live Quake state: %w", err)
	}
	if current.GameType != q3.GameTypeTDM && current.GameType != q3.GameTypeCTF {
		return BotCounts{}, errors.New("bot director requires Team Deathmatch or Capture the Flag")
	}
	if err := s.rconAllowTimeout("set bot_minplayers 0"); err != nil {
		return BotCounts{}, fmt.Errorf("could not disable Quake auto-fill: %w", err)
	}
	if err := s.rconAllowTimeout("kickbots"); err != nil {
		return BotCounts{}, err
	}
	if st, err := s.waitForBotRoster(0, nil, 5*time.Second); err != nil {
		return BotCounts{}, fmt.Errorf("could not confirm old bots were removed (%w); no bots were added", err)
	} else if botCounts(st.Players, 0).Total != 0 {
		return BotCounts{}, errors.New("could not confirm old bots were removed; no bots were added")
	}
	opponentTeam := "blue"
	if p.HumanTeam == "blue" {
		opponentTeam = "red"
	}
	expected := make([]string, 0, 2*p.BotsPerTeam)
	for _, entry := range []struct {
		names []string
		team  string
	}{{p.FriendlyBots[:p.BotsPerTeam], p.HumanTeam}, {p.OpponentBots[:p.BotsPerTeam], opponentTeam}} {
		for _, name := range entry.names {
			if err := s.rconAllowTimeout(fmt.Sprintf("addbot %s %d %s", safeToken(name), p.BaseSkill, entry.team)); err != nil {
				return BotCounts{}, err
			}
			// Quake can defer a bot connect. Confirm each named bot before sending
			// another addbot so we identify the exact rejected/deferred command and
			// never turn one missing bot into an ambiguous partial roster.
			expected = append(expected, name)
			if _, err := s.waitForBotRoster(len(expected), expected, 5*time.Second); err != nil {
				return BotCounts{}, fmt.Errorf("Quake did not add bot %q on %s: %w", name, entry.team, err)
			}
		}
	}
	st, err := s.waitForBotRoster(2*p.BotsPerTeam, expected, 5*time.Second)
	if err != nil {
		return BotCounts{}, fmt.Errorf("Quake did not report exactly %d named bots after rebuild: %w", 2*p.BotsPerTeam, err)
	}
	// addbot's third argument assigned each exact named bot to red or blue.
	// The stock status protocol cannot read that server-side team field back.
	return BotCounts{TargetPerTeam: p.BotsPerTeam, Red: p.BotsPerTeam, Blue: p.BotsPerTeam, Total: botCounts(st.Players, p.BotsPerTeam).Total, TeamsKnown: false}, nil
}

// ioquake3 occasionally executes a UDP RCON command but drops its reply while
// map state changes. For bot actions, final roster verification is authoritative:
// preserve a real transport/protocol error, but do not falsely fail solely on a
// timeout that the subsequent state read can confirm.
func (s *server) rconAllowTimeout(command string) error {
	_, err := s.rcon(command)
	if err == nil || isTimeout(err) {
		return nil
	}
	return err
}

func isTimeout(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func (s *server) reconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	s.mu.RLock()
	p := s.state.Policy
	s.mu.RUnlock()
	if !p.Enabled {
		http.Error(w, "bot policy disabled", 409)
		return
	}
	counts, err := s.rebuildBots(p)
	if err != nil {
		s.record("bot_reconcile", fmt.Sprintf("target=%d per team", p.BotsPerTeam), err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	detail := fmt.Sprintf("target=%d per team actual=%d red/%d blue", counts.TargetPerTeam, counts.Red, counts.Blue)
	s.record("bot_reconcile", detail, "confirmed")
	out(w, map[string]any{"ok": true, "bot_counts": counts})
}
func contains(a []string, x string) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}
func safeToken(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return -1
	}, s)
}
func (s *server) maps(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.availableCatalog()
	if err != nil {
		http.Error(w, "map inventory unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	out(w, catalog)
}

// availableMaps discovers installed assets at most once a minute. This keeps
// the selector responsive while making a newly added pk3 visible without a
// binary release or source-code whitelist.
func (s *server) availableMaps() ([]string, error) {
	catalog, err := s.availableCatalog()
	if err != nil {
		return nil, err
	}
	maps := make([]string, len(catalog))
	for i, info := range catalog {
		maps[i] = info.Name
	}
	return maps, nil
}

func (s *server) availableCatalog() ([]q3.MapInfo, error) {
	s.mu.RLock()
	if len(s.mapCatalog) > 0 && time.Since(s.mapCatalogAt) < time.Minute {
		catalog := append([]q3.MapInfo(nil), s.mapCatalog...)
		s.mu.RUnlock()
		return catalog, nil
	}
	s.mu.RUnlock()
	catalog, err := q3.DiscoverCatalog(s.cfg.GameDataPath)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.mapCatalog, s.mapCatalogAt = catalog, time.Now()
	s.mu.Unlock()
	return append([]q3.MapInfo(nil), catalog...), nil
}
func validGameType(gt int) bool { return gt == 0 || gt == 1 || gt == 3 || gt == 4 }
func gameTypeName(gt int) string {
	switch gt {
	case 0:
		return "Free For All"
	case 1:
		return "Tournament"
	case 3:
		return "Team Deathmatch"
	case 4:
		return "Capture the Flag"
	}
	return "Unknown"
}

func (s *server) loadMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		Map      string `json:"map"`
		GameType int    `json:"gametype"`
	}
	maps, mapsErr := s.availableMaps()
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || mapsErr != nil || !q3.ContainsMap(maps, in.Map) || !validGameType(in.GameType) {
		http.Error(w, "invalid map or gametype", http.StatusBadRequest)
		return
	}

	// Change the mode and load separately. ioquake3 defers g_gametype changes
	// until a restart, but map itself restarts the game; putting both in one
	// RCON datagram can leave only the first command applied.
	if _, err := s.rcon(fmt.Sprintf("set g_gametype %d", in.GameType)); err != nil {
		s.record("map_load", fmt.Sprintf("%s type=%d", in.Map, in.GameType), err.Error())
		http.Error(w, "could not set gametype: "+err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := s.rcon("map " + in.Map); err != nil {
		s.record("map_load", fmt.Sprintf("%s type=%d", in.Map, in.GameType), err.Error())
		http.Error(w, "could not send map load: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Custom maps can pause RCON while BSP/assets and bot navigation initialize.
	// Keep polling through temporary UDP timeouts, then require live confirmation
	// before we report the action as successful.
	const attempts = 30
	for attempt := 0; attempt < attempts; attempt++ {
		time.Sleep(time.Second)
		status, err := s.live()
		if err == nil && status.Map == in.Map && status.GameType == in.GameType {
			s.record("map_load", fmt.Sprintf("%s type=%d", in.Map, in.GameType), "confirmed")
			out(w, map[string]any{"ok": true, "map": status.Map, "gametype": status.GameType})
			return
		}
	}
	detail := fmt.Sprintf("%s type=%d was not observed within 30 seconds after map load", in.Map, in.GameType)
	s.record("map_load", detail, "unconfirmed")
	http.Error(w, detail, http.StatusBadGateway)
}

func (s *server) restartMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	_, e := s.rcon("map_restart 5")
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	s.record("map_restart", "5 second countdown", "ok")
	out(w, map[string]bool{"ok": true})
}
func (s *server) rotation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out(w, s.state.Rotation)
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(405)
		return
	}
	var x q3.Rotation
	maps, mapsErr := s.availableMaps()
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&x) != nil || mapsErr != nil || x.ValidateWithMaps(maps) != nil {
		http.Error(w, "invalid rotation", 400)
		return
	}
	s.mu.Lock()
	s.state.Rotation = x
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "could not persist rotation", http.StatusInternalServerError)
		return
	}
	s.record("rotation", "updated stored rotation", "ok")
	out(w, x)
}

// applyRotation installs the saved d1..dN chain for the next map only. It
// deliberately does not issue a map command, so the current match continues.
func (s *server) applyRotation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	rotation := append(q3.Rotation(nil), s.state.Rotation...)
	s.mu.RUnlock()
	maps, err := s.availableMaps()
	if err != nil {
		http.Error(w, "map inventory unavailable", http.StatusServiceUnavailable)
		return
	}
	if err = rotation.ValidateWithMaps(maps); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	commands, err := rotation.NextMapCommandListWithMaps(maps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for index, command := range commands {
		if _, err = s.rcon(command); err != nil {
			detail := fmt.Sprintf("entry %d of %d", index+1, len(commands))
			s.record("rotation_apply", detail, err.Error())
			http.Error(w, "could not install rotation "+detail+": "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Reading the cvar confirms the final nextmap assignment rather than merely
	// treating the last UDP reply as proof that the chain survived parsing.
	verify, err := s.rcon("nextmap")
	if err != nil || !strings.Contains(verify, "vstr d1") {
		detail := "nextmap was not confirmed as vstr d1"
		if err != nil {
			detail += ": " + err.Error()
		}
		s.record("rotation_apply", detail, "unconfirmed")
		http.Error(w, detail, http.StatusBadGateway)
		return
	}
	s.record("rotation_apply", fmt.Sprintf("%d entries installed and nextmap confirmed", len(rotation)), "confirmed")
	out(w, map[string]any{"ok": true, "applies": "next map", "rotation": rotation})
}
func (s *server) announce(w http.ResponseWriter, r *http.Request) {
	var x struct {
		Message string `json:"message"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&x) != nil || len(x.Message) < 1 || len(x.Message) > 140 {
		http.Error(w, "invalid message", 400)
		return
	}
	_, e := s.rcon("say " + safeMessage(x.Message))
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	s.record("announce", x.Message, "ok")
	out(w, map[string]bool{"ok": true})
}
func safeMessage(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}
func (s *server) kick(w http.ResponseWriter, r *http.Request) {
	var x struct {
		ID int `json:"id"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 512)).Decode(&x) != nil || x.ID < 0 || x.ID > 63 {
		http.Error(w, "invalid player id", 400)
		return
	}
	_, e := s.rcon(fmt.Sprintf("clientkick %d", x.ID))
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	s.record("player_kick", strconv.Itoa(x.ID), "ok")
	out(w, map[string]bool{"ok": true})
}
func (s *server) audit(w http.ResponseWriter, r *http.Request) { out(w, s.readAudit()) }
func (s *server) record(action, detail, result string) {
	a := Audit{time.Now().UTC(), action, detail, result}
	b, _ := json.Marshal(a)
	_ = os.MkdirAll(filepath.Dir(s.cfg.AuditFile), 0750)
	f, e := os.OpenFile(s.cfg.AuditFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if e == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	s.mu.RLock()
	for ch := range s.subscribers {
		select {
		case ch <- a:
		default:
		}
	}
	s.mu.RUnlock()
}
func (s *server) readAudit() []Audit {
	f, e := os.Open(s.cfg.AuditFile)
	if e != nil {
		return []Audit{}
	}
	defer f.Close()
	var outp []Audit
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var a Audit
		if json.Unmarshal(sc.Bytes(), &a) == nil {
			outp = append(outp, a)
		}
	}
	if len(outp) > 80 {
		outp = outp[len(outp)-80:]
	}
	return outp
}

// readGameLog reads only the configured server-side log file. It intentionally
// never accepts a path from the browser. On a new SSE connection it sends the
// most recent 128 KiB; later calls return only appended lines.
func readGameLog(path string, offset *int64, initial bool) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() < *offset {
		*offset = 0 // log rotation/truncation
	}
	start := *offset
	skipPartial := false
	if initial && start == 0 && info.Size() > 128*1024 {
		start = info.Size() - 128*1024
		skipPartial = true
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 256*1024)
	if skipPartial {
		// The initial tail might begin halfway through a log line.
		sc.Scan()
	}
	lines := make([]string, 0)
	for sc.Scan() {
		line := sanitizeLogLine(sc.Text())
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	if err = sc.Err(); err != nil {
		return nil, err
	}
	*offset = info.Size()
	return lines, nil
}

func sanitizeLogLine(line string) string {
	line = strings.Map(func(r rune) rune {
		if r == '	' || r >= ' ' {
			return r
		}
		return -1
	}, line)
	if len(line) > 4096 {
		return line[:4096] + " [truncated]"
	}
	return line
}

func writeSSE(w io.Writer, event string, value any) {
	b, _ := json.Marshal(value)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan Audit, 16)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, ch); s.mu.Unlock() }()
	writeSSE(w, "connected", map[string]string{"message": "game and control log stream connected"})
	for _, a := range s.readAudit() {
		writeSSE(w, "audit", a)
	}
	var gameOffset int64
	if lines, err := readGameLog(s.cfg.GameLogFile, &gameOffset, true); err != nil {
		writeSSE(w, "game", map[string]string{"line": "[game log unavailable: waiting for " + s.cfg.GameLogFile + "]"})
	} else {
		for _, line := range lines {
			writeSSE(w, "game", map[string]string{"line": line})
		}
	}
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	tail := time.NewTicker(time.Second)
	defer ping.Stop()
	defer tail.Stop()
	for {
		select {
		case a := <-ch:
			writeSSE(w, "audit", a)
			fl.Flush()
		case <-tail.C:
			if lines, err := readGameLog(s.cfg.GameLogFile, &gameOffset, false); err == nil {
				for _, line := range lines {
					writeSSE(w, "game", map[string]string{"line": line})
				}
				if len(lines) > 0 {
					fl.Flush()
				}
			}
		case <-ping.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func (s *server) loadState() {
	x, err := (store.File[Persisted]{Path: s.cfg.StateFile}).Load()
	if err != nil || validatePolicy(x.Policy) != nil {
		return
	}
	maps, catalogErr := s.availableMaps()
	if catalogErr == nil && x.Rotation.ValidateWithMaps(maps) == nil {
		s.state = x
	}
}
func (s *server) saveLocked() error {
	return (store.File[Persisted]{Path: s.cfg.StateFile}).Save(s.state)
}
func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, strings.ReplaceAll(html, "__CSRF__", s.csrfToken))
}

var html = `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>q3ctl</title><style>:root{color-scheme:dark}body{font:15px system-ui;background:#10151c;color:#e8edf3;margin:0}main{max-width:1200px;margin:auto;padding:22px}h1{margin:0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(330px,1fr));gap:14px;margin:14px 0}.card{background:#18212c;border:1px solid #304050;border-radius:10px;padding:14px}button,select,input{font:inherit;padding:8px;margin:3px}button{background:#2563eb;color:white;border:0;border-radius:6px;cursor:pointer;transition:transform .08s,filter .12s,opacity .12s}button:hover{filter:brightness(1.12)}button:active,button.pressed{transform:translateY(2px) scale(.98);filter:brightness(.8)}button:disabled{opacity:.62;cursor:wait}button.danger{background:#b91c1c}pre{white-space:pre-wrap;background:#090d12;padding:12px;max-height:280px;overflow:auto;border-radius:6px}.rotation-row{display:flex;align-items:center;gap:6px;flex-wrap:wrap;padding:7px 0;border-bottom:1px solid #304050}.rotation-row:last-child{border-bottom:0}.rotation-row b{min-width:24px}.rotation-row .limits{display:flex;gap:4px;align-items:center}.rotation-row .limits label{margin:0;font-size:12px;color:#afbdcc}.rotation-row .limits input{width:4.2em;padding:5px;margin:0}.pill{padding:3px 7px;border-radius:12px;background:#28435d}label{display:block;margin:5px 0}</style><main><h1>Quake 3 Control</h1><p>Private Tailscale dashboard · Server-local RCON</p><div class=grid><section class=card><h2>Live server</h2><div id=summary>Loading…</div><button onclick=refresh()>Refresh status</button><button onclick=post('/api/v1/maps/restart',{})>Restart map (5s)</button><h3>Players</h3><div id=players></div></section><section class=card><h2>Map & mode</h2><label>Map <select id=map></select></label><label>Mode <select id=mode><option value=0>Free For All</option><option value=1>Tournament</option><option value=3>Team Deathmatch</option><option value=4>Capture the Flag</option></select></label><button onclick=loadMap()>Load map</button><h3>Announcement</h3><input id=msg maxlength=140 placeholder="Message to all players"><button onclick=announce()>Send</button></section><section class=card><h2>Bot director</h2><label>Human team <select id=team><option>red</option><option>blue</option></select></label><label>Bots/team <input id=bots type=number min=0 max=4></label><label>Base skill <input id=skill type=number min=1 max=5></label><label><input id=adaptive type=checkbox> adaptive next-round policy</label><div id=botActual class=pill>Actual bots: loading…</div><button onclick=savePolicy()>Save & apply policy</button><button onclick=reconcile()>Rebuild bot teams</button><p><small>Saving applies exactly the selected bots per team; automatic Quake bot filling is disabled.</small></p></section><section class=card><h2>Rotation</h2><div id=rotation></div><button onclick=addRotation()>Add map</button><button onclick=saveRotation()>Save rotation</button><button onclick=applyRotation()>Apply at next map</button><p><small>Save persists the definition atomically. Apply installs it as Quake's next-map chain without interrupting the current match.</small></p></section></div><section class=card><h2>Live game log</h2><pre id=gameLog>Connecting…</pre></section><section class=card><h2>q3ctl control log</h2><pre id=auditLog>Connecting…</pre></section></main><script>const csrf='__CSRF__';let state,mapCatalog=[],policyHydrated=false;const $=id=>document.getElementById(id);function setBusy(button,busy){if(!button)return;button.disabled=busy;button.classList.toggle('pressed',busy);if(busy){button.dataset.label=button.textContent;button.textContent='Working…'}else if(button.dataset.label){button.textContent=button.dataset.label;delete button.dataset.label}}let activeButton=null;document.addEventListener('click',e=>{let b=e.target.closest('button');if(!b)return;activeButton=b;b.classList.add('pressed');setTimeout(()=>b.classList.remove('pressed'),180)},true);async function api(u,o={}){let h=new Headers(o.headers||{});let mutation=o.method&&o.method!=='GET';if(mutation)h.set('X-CSRF-Token',csrf);let button=mutation?activeButton:null;if(button)setBusy(button,true);try{let r=await fetch(u,{...o,headers:h});if(!r.ok)throw Error(await r.text());return r.json()}finally{if(button)setBusy(button,false)}}async function post(u,b){try{await api(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)})}catch(e){alert(e)}finally{refresh()}}function esc(x){return String(x).replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))}function modeName(x){return ({0:'FFA',1:'Tournament',3:'TDM',4:'CTF'})[x]||'Unknown'}function playerLine(p){return '<div><span class=pill>'+p.id+'</span> '+esc(p.name)+' '+(p.bot?'BOT':'HUMAN')+' score '+p.score+' <button class=danger onclick="kick('+p.id+')">Kick</button></div>'}function renderPlayers(players,gametype){if(!players.length)return 'Nobody connected';if(gametype!==3&&gametype!==4||!players.some(p=>p.team))return players.map(playerLine).join('');let groups={red:[],blue:[],spectator:[],other:[]};players.forEach(p=>(groups[p.team]||groups.other).push(p));return ['red','blue','spectator','other'].filter(team=>groups[team].length).map(team=>'<h4>'+team[0].toUpperCase()+team.slice(1)+'</h4>'+groups[team].map(playerLine).join('')).join('')}function renderMapSelect(){let select=$('map'),previous=select.value,mode=+$('mode').value;let matching=mapCatalog.filter(info=>Array.isArray(info.gametypes)&&info.gametypes.includes(mode));select.innerHTML=matching.map(info=>'<option value="'+esc(info.name)+'">'+esc(info.name)+'</option>').join('');if([...select.options].some(option=>option.value===previous))select.value=previous;else if(!matching.length)select.innerHTML='<option value="">No installed maps declared for '+modeName(mode)+'</option>'}function append(id,text){let e=$(id);let follow=e.scrollHeight-e.scrollTop-e.clientHeight<24;e.textContent+=text+'\n';if(e.textContent.length>128000)e.textContent=e.textContent.slice(-96000);if(follow)e.scrollTop=e.scrollHeight}async function refresh(){try{state=await api('/api/v1/status');let s=state.server;let h=state.host||{};let mem=h.memory_total_bytes?Math.round(100*h.memory_available_bytes/h.memory_total_bytes):0;$('summary').innerHTML='<b>'+esc(s.map)+'</b> · '+modeName(s.gametype)+' · '+s.players.length+'/'+s.max_clients+' players<br><small>Host load '+(+h.load_1||0).toFixed(2)+' · memory '+mem+'% free</small>';$('players').innerHTML=renderPlayers(s.players,s.gametype);let bc=state.bot_counts||{};let botText='Actual bots: '+(bc.total??0)+' total · target '+(bc.target_per_team??0)+' per team';if(bc.teams_known)botText+=' · '+(bc.red??0)+' red / '+(bc.blue??0)+' blue';else botText+=' · team readback unavailable in stock ioquake3';$('botActual').textContent=botText;if(!policyHydrated){$('team').value=state.policy.human_team;$('bots').value=state.policy.bots_per_team;$('skill').value=state.policy.base_skill;$('adaptive').checked=state.policy.adaptive;policyHydrated=true}renderRotation()}catch(e){$('summary').textContent='Status unavailable: '+e}}async function kick(id){if(confirm('Kick client '+id+'?'))await post('/api/v1/players/kick',{id})}async function loadMap(){if(confirm('Load selected map now?'))await post('/api/v1/maps/load',{map:$('map').value,gametype:+$('mode').value})}async function announce(){await post('/api/v1/announce',{message:$('msg').value});$('msg').value=''}async function savePolicy(){let p={...state.policy,human_team:$('team').value,bots_per_team:+$('bots').value,base_skill:+$('skill').value,adaptive:$('adaptive').checked};try{await api('/api/v1/bots/policy',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)})}catch(e){alert(e)}finally{refresh()}}async function reconcile(){if(confirm('Kick and rebuild all bots on configured teams?'))await post('/api/v1/bots/reconcile',{})}function allMapOptions(){return mapCatalog.map(info=>'<option value="'+esc(info.name)+'">'+esc(info.name)+'</option>').join('')}function rotationRow(r,i){let maps=allMapOptions();return '<div class=rotation-row data-rotation-row='+i+'><b>#'+(i+1)+'</b><select class=rot-map>'+maps+'</select><select class=rot-mode><option value=0>FFA</option><option value=1>Tournament</option><option value=3>TDM</option><option value=4>CTF</option></select><span class=limits><label>Time <input class=rot-time type=number min=0 max=120 value='+r.timelimit+'></label><label>Frags <input class=rot-frags type=number min=0 max=999 value='+r.fraglimit+'></label><label>Caps <input class=rot-caps type=number min=0 max=99 value='+r.capturelimit+'></label></span><button class=danger onclick=removeRotation('+i+')>Remove</button></div>'}function renderRotation(){let root=$('rotation');root.innerHTML=state.rotation.map(rotationRow).join('');[...root.querySelectorAll('[data-rotation-row]')].forEach((row,i)=>{row.querySelector('.rot-map').value=state.rotation[i].map;row.querySelector('.rot-mode').value=state.rotation[i].gametype})}function addRotation(){if(state.rotation.length>=12)return alert('A rotation can contain at most 12 maps');state.rotation.push({map:'q3ctf1',gametype:4,timelimit:20,fraglimit:0,capturelimit:8});renderRotation()}function removeRotation(i){if(state.rotation.length===1)return alert('A rotation needs at least one map');state.rotation.splice(i,1);renderRotation()}function readRotation(){return [...$('rotation').querySelectorAll('[data-rotation-row]')].map(row=>({map:row.querySelector('.rot-map').value,gametype:+row.querySelector('.rot-mode').value,timelimit:+row.querySelector('.rot-time').value,fraglimit:+row.querySelector('.rot-frags').value,capturelimit:+row.querySelector('.rot-caps').value}))}async function saveRotation(){try{state.rotation=readRotation();state.rotation=await api('/api/v1/rotation',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(state.rotation)});renderRotation();alert('Rotation saved permanently')}catch(e){alert(e)}}async function applyRotation(){try{if(!confirm('Install the saved rotation for the next map? The current match will continue.'))return;let r=await api('/api/v1/rotation/apply',{method:'POST'});alert('Rotation installed: '+r.rotation.length+' maps will start at the next map')}catch(e){alert(e)}}async function init(){
  $('mode').addEventListener('change',renderMapSelect);
  startLogs();
  await refresh();
  if(state&&state.server&&validGameType(state.server.gametype))$('mode').value=state.server.gametype;
  try{mapCatalog=await api('/api/v1/maps');renderMapSelect();renderRotation()}catch(e){$('map').innerHTML='<option value="">Map catalog unavailable: '+esc(e)+'</option>';renderRotation()}
}
function validGameType(x){return x===0||x===1||x===3||x===4}
function startLogs(){let es=new EventSource('/api/v1/logs/stream');es.addEventListener('connected',e=>{let m='[connected] '+JSON.parse(e.data).message;$('gameLog').textContent=m+'\n';$('auditLog').textContent=m+'\n'});es.addEventListener('game',e=>append('gameLog',JSON.parse(e.data).line));es.addEventListener('audit',e=>append('auditLog',JSON.stringify(JSON.parse(e.data))));es.onerror=()=>{append('gameLog','[stream reconnecting]');append('auditLog','[stream reconnecting]')}}init()</script>`
