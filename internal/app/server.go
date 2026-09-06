// Package app contains q3ctl's HTTP API and supervised runtime.
package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
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
	FriendlyFire     bool     `json:"friendly_fire"`
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
	botOps       chan struct{}
	rconMu       sync.Mutex
	rconLast     time.Time
	state        Persisted
	subscribers  map[chan Audit]struct{}
	csrfToken    string
	mapCatalog   []q3.MapInfo
	mapCatalogAt time.Time
	lastGameType int
	lastGameAt   time.Time
}

func defaults() Persisted {
	return Persisted{Policy: BotPolicy{Enabled: true, HumansSingleTeam: true, HumanTeam: "red", BotsPerTeam: 4, FriendlyBots: []string{"Sarge", "Major", "Grunt", "Crash"}, OpponentBots: []string{"Hunter", "Xaero", "Bitterman", "TankJr"}, BaseSkill: 3, Adaptive: true, MinSkill: 1, MaxSkill: 5, FriendlyFire: false}, Rotation: q3.Rotation{{Map: "q3ctf1", GameType: 4, TimeLimit: 20, FragLimit: 0, CaptureLimit: 8}, {Map: "q3ctf2", GameType: 4, TimeLimit: 20, FragLimit: 0, CaptureLimit: 8}, {Map: "q3dm6", GameType: 3, TimeLimit: 15, FragLimit: 40, CaptureLimit: 0}, {Map: "q3dm17", GameType: 3, TimeLimit: 15, FragLimit: 40, CaptureLimit: 0}}}
}
func New(cfg config.Config) *server {
	s := &server{cfg: cfg, state: defaults(), subscribers: map[chan Audit]struct{}{}, csrfToken: newCSRFToken(), botOps: make(chan struct{}, 1)}
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
	mux.HandleFunc("/api/v1/bots/add", s.addBot)
	mux.HandleFunc("/api/v1/bots/rebalance", s.rebalanceBots)
	mux.HandleFunc("/api/v1/gameplay/friendly-fire", s.friendlyFire)
	mux.HandleFunc("/api/v1/gameplay/limits", s.matchLimits)
	mux.HandleFunc("/api/v1/maps", s.maps)
	mux.HandleFunc("/api/v1/maps/load", s.loadMap)
	mux.HandleFunc("/api/v1/maps/restart", s.restartMap)
	mux.HandleFunc("/api/v1/maps/next", s.nextMap)
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
	s.rconMu.Lock()
	defer s.rconMu.Unlock()
	// ioquake3 rate-limits connectionless RCON packets by source address. Keep
	// unrelated dashboard refreshes from starving a bot rebuild's next command.
	if wait := 300*time.Millisecond - time.Since(s.rconLast); wait > 0 {
		time.Sleep(wait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reply, err := (rcon.Client{Address: s.cfg.RCONAddr, Password: s.cfg.RCONPassword}).Execute(ctx, command)
	s.rconLast = time.Now()
	return reply, err
}

// getStatusOnce shares the connectionless packet gate with RCON. ioquake3
// limits both request classes by source IP, so an overlapping dashboard refresh
// must not inject a getstatus packet in the middle of a policy application.
func (s *server) getStatusOnce(timeout time.Duration) (string, error) {
	s.rconMu.Lock()
	defer s.rconMu.Unlock()
	if wait := 300*time.Millisecond - time.Since(s.rconLast); wait > 0 {
		time.Sleep(wait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reply, err := (rcon.Client{Address: s.cfg.RCONAddr}).GetStatus(ctx)
	s.rconLast = time.Now()
	return reply, err
}
func parseStatus(raw string) Status {
	// A status response with no parseable player rows is valid (for example
	// during a connect/disconnect transition). Keep the JSON contract stable:
	// callers must receive players:[] rather than players:null.
	st := Status{Raw: raw, Players: make([]Player, 0)}
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

func (s *server) roster() (Status, error) {
	raw, err := s.rcon("status")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(raw), nil
}

// waitForBotRoster uses only the engine's authenticated status table. Avoid
// getstatus here: a bot rebuild has already confirmed TDM/CTF once, while a
// second UDP protocol in every poll adds rate-limit pressure without helping
// us decide whether the requested bot process exists.
func (s *server) waitForBotRoster(want int, names []string, timeout time.Duration) (Status, error) {
	deadline := time.Now().Add(timeout)
	var last Status
	var lastErr error
	for time.Now().Before(deadline) {
		st, err := s.roster()
		if err == nil {
			last = st
			if botCounts(st.Players, 0).Total == want && (want == 0 || hasExpectedBotNames(st.Players, names)) {
				return st, nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(800 * time.Millisecond)
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
	st.TimeLimit, _ = strconv.Atoi(values["timelimit"])
	st.FragLimit, _ = strconv.Atoi(values["fraglimit"])
	st.CaptureLimit, _ = strconv.Atoi(values["capturelimit"])
	st.MaxClients, _ = strconv.Atoi(values["sv_maxclients"])
	s.rememberGameType(gameType)
	s.populateTeamsFromGameLog(&st)
	return st, nil
}

func (s *server) rememberGameType(gameType int) {
	if !validGameType(gameType) {
		return
	}
	s.mu.Lock()
	s.lastGameType, s.lastGameAt = gameType, time.Now()
	s.mu.Unlock()
}

// cachedGameTypeLocked requires s.mu to be held by the caller.
func (s *server) cachedGameTypeLocked(now time.Time) (int, bool) {
	return s.lastGameType, validGameType(s.lastGameType) && now.Sub(s.lastGameAt) < 2*time.Minute
}

// botGameType avoids making Save & apply depend on a second unauthenticated
// status request. The authenticated RCON status response is sufficient when it
// carries g_gametype; otherwise use a recent, previously confirmed live mode.
// Only a cold controller with no usable RCON/cached mode needs getstatus.
func (s *server) botGameType() (int, error) {
	st, err := s.roster()
	if err != nil {
		return 0, err
	}
	if validGameType(st.GameType) && strings.Contains(st.Raw, `\g_gametype\`) {
		return st.GameType, nil
	}
	s.mu.RLock()
	gameType, ok := s.cachedGameTypeLocked(time.Now())
	s.mu.RUnlock()
	if ok {
		return gameType, nil
	}
	raw, err := s.getStatusInfo()
	if err != nil {
		return 0, err
	}
	values := parseInfoString(raw)
	gameType, err = strconv.Atoi(values["g_gametype"])
	if err != nil || !validGameType(gameType) {
		return 0, errors.New("Quake gametype was not understood")
	}
	s.rememberGameType(gameType)
	return gameType, nil
}

// populateTeamsFromGameLog reads the game VM's authoritative player
// configstring snapshot. Unlike the engine-level dumpuser command, the
// ClientUserinfoChanged log record writes sess.sessionTeam as t=1/2/3.
func (s *server) populateTeamsFromGameLog(st *Status) {
	if st.GameType != q3.GameTypeTDM && st.GameType != q3.GameTypeCTF {
		return
	}
	var offset int64
	lines, err := readGameLog(s.cfg.GameLogFile, &offset, true)
	if err != nil {
		return
	}
	teams := map[int]string{}
	userInfo := regexp.MustCompile(`ClientUserinfoChanged:\s*(\d+)\s+(.+)$`)
	for _, line := range lines {
		match := userInfo.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		id, _ := strconv.Atoi(match[1])
		fields := strings.Split(strings.TrimPrefix(match[2], "\\"), "\\")
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "t" {
				teams[id] = map[string]string{"1": "red", "2": "blue", "3": "spectator"}[fields[i+1]]
				break
			}
		}
	}
	for i := range st.Players {
		st.Players[i].Team = teams[st.Players[i].ID]
	}
}

func teamDataComplete(st Status) bool {
	if st.GameType != q3.GameTypeTDM && st.GameType != q3.GameTypeCTF {
		return false
	}
	for _, player := range st.Players {
		if player.Team == "" {
			return false
		}
	}
	return len(st.Players) > 0
}

// getStatusInfo tolerates one transient UDP loss. A single localhost UDP
// timeout is not evidence that Quake changed mode or stopped serving; retrying
// is bounded and remains below ioquake3's getstatus rate limit.
func (s *server) getStatusInfo() (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := s.getStatusOnce(2 * time.Second)
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
	counts := botCounts(x.Players, p.Policy.BotsPerTeam)
	counts.TeamsKnown = teamDataComplete(x)
	out(w, map[string]any{"server": x, "policy": p.Policy, "bot_counts": counts, "rotation": p.Rotation, "host": hostStats()})
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
	if !s.beginBotOperation() {
		http.Error(w, "another bot operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
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

type friendlyFireRequest struct {
	Enabled bool `json:"enabled"`
}

// friendlyFire is deliberately separate from bot-policy reconciliation. It
// changes the live game rule immediately without kicking or respawning bots.
// g_friendlyFire is an archived ioquake3 VM cvar, while q3ctl also persists
// the selected value so the dashboard accurately reflects its last command.
func (s *server) friendlyFire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request friendlyFireRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil {
		http.Error(w, "invalid friendly-fire request", http.StatusBadRequest)
		return
	}
	if !s.beginBotOperation() {
		http.Error(w, "another bot or gameplay operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
	value := "0"
	state := "off"
	if request.Enabled {
		value, state = "1", "on"
	}
	if _, err := s.rcon("set g_friendlyFire " + value); err != nil {
		s.record("friendly_fire", "requested="+state, err.Error())
		http.Error(w, "Quake did not accept friendly-fire change: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	s.state.Policy.FriendlyFire = request.Enabled
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		s.record("friendly_fire", "live="+state, "could not persist dashboard state: "+err.Error())
		http.Error(w, "friendly fire changed live but q3ctl could not persist the setting", http.StatusInternalServerError)
		return
	}
	s.record("friendly_fire", "live="+state, "applied")
	out(w, map[string]bool{"enabled": request.Enabled})
}

type matchLimitsRequest struct {
	TimeLimit    int `json:"timelimit"`
	FragLimit    int `json:"fraglimit"`
	CaptureLimit int `json:"capturelimit"`
}

func validateMatchLimits(x matchLimitsRequest) error {
	if x.TimeLimit < 0 || x.TimeLimit > 120 {
		return errors.New("timelimit must be between 0 and 120")
	}
	if x.FragLimit < 0 || x.FragLimit > 999 {
		return errors.New("fraglimit must be between 0 and 999")
	}
	if x.CaptureLimit < 0 || x.CaptureLimit > 99 {
		return errors.New("capturelimit must be between 0 and 99")
	}
	return nil
}

// matchLimits changes the active game's serverinfo cvars. ioquake3 marks
// these CVAR_NORESTART and updates them each game frame, so this takes effect
// now rather than waiting for the following map.
func (s *server) matchLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request matchLimitsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil || validateMatchLimits(request) != nil {
		http.Error(w, "invalid match limits", http.StatusBadRequest)
		return
	}
	if !s.beginBotOperation() {
		http.Error(w, "another bot or gameplay operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
	for _, command := range []string{
		fmt.Sprintf("set timelimit %d", request.TimeLimit),
		fmt.Sprintf("set fraglimit %d", request.FragLimit),
		fmt.Sprintf("set capturelimit %d", request.CaptureLimit),
	} {
		if _, err := s.rcon(command); err != nil {
			s.record("match_limits", fmt.Sprintf("time=%d frag=%d capture=%d", request.TimeLimit, request.FragLimit, request.CaptureLimit), err.Error())
			http.Error(w, "Quake did not accept match-limit change; earlier values may have applied: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	st, err := s.live()
	if err != nil || st.TimeLimit != request.TimeLimit || st.FragLimit != request.FragLimit || st.CaptureLimit != request.CaptureLimit {
		detail := "match limits were not confirmed"
		if err != nil {
			detail += ": " + err.Error()
		}
		s.record("match_limits", fmt.Sprintf("time=%d frag=%d capture=%d", request.TimeLimit, request.FragLimit, request.CaptureLimit), detail)
		http.Error(w, detail, http.StatusBadGateway)
		return
	}
	s.record("match_limits", fmt.Sprintf("time=%d frag=%d capture=%d", request.TimeLimit, request.FragLimit, request.CaptureLimit), "applied and confirmed")
	out(w, st)
}

type addBotRequest struct {
	Name  string `json:"name"`
	Team  string `json:"team"`
	Skill int    `json:"skill"`
}

func configuredBotName(p BotPolicy, name string) (string, bool) {
	for _, candidate := range append(append([]string{}, p.FriendlyBots...), p.OpponentBots...) {
		if strings.EqualFold(candidate, strings.TrimSpace(name)) {
			return candidate, true
		}
	}
	return "", false
}

func botNames(players []Player) []string {
	names := make([]string, 0)
	for _, player := range players {
		if player.Bot {
			names = append(names, player.Name)
		}
	}
	return names
}

// addBot is an ad-hoc, verified addition. It does not alter the persisted
// Bots/team target: a later rebuild intentionally restores that exact policy.
func (s *server) addBot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request addBotRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil || (request.Team != "red" && request.Team != "blue") || request.Skill < 1 || request.Skill > 5 {
		http.Error(w, "invalid bot request", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	policy := s.state.Policy
	s.mu.RUnlock()
	name, allowed := configuredBotName(policy, request.Name)
	if !allowed {
		http.Error(w, "bot is not in the configured bot roster", http.StatusBadRequest)
		return
	}
	if !s.beginBotOperation() {
		http.Error(w, "another bot or gameplay operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
	gameType, err := s.botGameType()
	if err != nil {
		http.Error(w, "could not determine live Quake mode: "+err.Error(), http.StatusBadGateway)
		return
	}
	if gameType != q3.GameTypeTDM && gameType != q3.GameTypeCTF {
		http.Error(w, "adding a team bot requires Team Deathmatch or Capture the Flag", http.StatusConflict)
		return
	}
	st, err := s.roster()
	if err != nil {
		http.Error(w, "could not read current bot roster: "+err.Error(), http.StatusBadGateway)
		return
	}
	expected := botNames(st.Players)
	for _, existing := range expected {
		if strings.EqualFold(existing, name) {
			http.Error(w, "that bot is already connected", http.StatusConflict)
			return
		}
	}
	expected = append(expected, name)
	if err := s.addAndConfirmBot(name, request.Team, request.Skill, expected); err != nil {
		s.record("bot_add", fmt.Sprintf("name=%s team=%s skill=%d", name, request.Team, request.Skill), err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	confirmed, err := s.roster()
	if err != nil {
		http.Error(w, "bot was added but final roster read failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	counts := botCounts(confirmed.Players, policy.BotsPerTeam)
	s.record("bot_add", fmt.Sprintf("name=%s team=%s skill=%d", name, request.Team, request.Skill), "confirmed")
	out(w, map[string]any{"ok": true, "bot": name, "team": request.Team, "skill": request.Skill, "bot_counts": counts})
}

// rebalanceBots moves only existing bot clients. It requires complete,
// game-VM-sourced team data and refuses to act if the current humans alone
// cannot be balanced without moving a human.
func (s *server) rebalanceBots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.beginBotOperation() {
		http.Error(w, "another bot or gameplay operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
	st, err := s.live()
	if err != nil {
		http.Error(w, "could not read live teams: "+err.Error(), http.StatusBadGateway)
		return
	}
	if st.GameType != q3.GameTypeTDM && st.GameType != q3.GameTypeCTF {
		http.Error(w, "bot rebalancing requires Team Deathmatch or Capture the Flag", http.StatusConflict)
		return
	}
	if !teamDataComplete(st) {
		http.Error(w, "authoritative team data is incomplete; no clients were moved", http.StatusConflict)
		return
	}
	moves, err := botRebalanceMoves(st.Players)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if len(moves) == 0 {
		out(w, map[string]any{"ok": true, "moves": 0, "message": "teams already balanced"})
		return
	}
	for _, move := range moves {
		if _, err := s.rcon(fmt.Sprintf("forceteam %d %s", move.ID, move.Team)); err != nil {
			s.record("bot_rebalance", fmt.Sprintf("bot=%d team=%s", move.ID, move.Team), err.Error())
			http.Error(w, "bot rebalance stopped; some earlier bot moves may have applied: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// The VM logs ClientUserinfoChanged after a forceteam operation. Poll that
	// authoritative data rather than claiming success from the RCON reply alone.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		confirmed, readErr := s.live()
		if readErr == nil && teamDataComplete(confirmed) {
			if remaining, moveErr := botRebalanceMoves(confirmed.Players); moveErr == nil && len(remaining) == 0 {
				s.record("bot_rebalance", fmt.Sprintf("moved=%d bots", len(moves)), "confirmed")
				out(w, map[string]any{"ok": true, "moves": len(moves), "server": confirmed})
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	http.Error(w, "bot moves were sent but the balanced team state was not confirmed", http.StatusBadGateway)
}

type botTeamMove struct {
	ID   int
	Team string
}

// botRebalanceMoves returns the smallest set of bot-only transfers needed to
// bring red/blue total player counts within one. It never moves a human and
// fails closed if the human distribution makes that impossible.
func botRebalanceMoves(players []Player) ([]botTeamMove, error) {
	var redHumans, blueHumans int
	var redBots, blueBots []Player
	for _, player := range players {
		switch player.Team {
		case "red":
			if player.Bot {
				redBots = append(redBots, player)
			} else {
				redHumans++
			}
		case "blue":
			if player.Bot {
				blueBots = append(blueBots, player)
			} else {
				blueHumans++
			}
		}
	}
	redTotal, blueTotal := redHumans+len(redBots), blueHumans+len(blueBots)
	if abs(redHumans-blueHumans) > len(redBots)+len(blueBots)+1 {
		return nil, errors.New("human teams are too uneven to balance without moving a human")
	}
	if abs(redTotal-blueTotal) <= 1 {
		return nil, nil
	}
	from, to := redBots, "blue"
	difference := redTotal - blueTotal
	if difference < 0 {
		from, to, difference = blueBots, "red", -difference
	}
	movesNeeded := difference / 2
	if len(from) < movesNeeded {
		return nil, errors.New("teams cannot be balanced without moving a human")
	}
	moves := make([]botTeamMove, 0, movesNeeded)
	for _, bot := range from[:movesNeeded] {
		moves = append(moves, botTeamMove{ID: bot.ID, Team: to})
	}
	return moves, nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (s *server) beginBotOperation() bool {
	select {
	case s.botOps <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *server) endBotOperation() { <-s.botOps }

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
	gameType, err := s.botGameType()
	if err != nil {
		return BotCounts{}, fmt.Errorf("could not determine live Quake mode: %w", err)
	}
	if gameType != q3.GameTypeTDM && gameType != q3.GameTypeCTF {
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

	// From this point a failed rebuild must leave the server clean. A partial
	// roster makes the selected target look applied while a later addbot was
	// lost or rejected.
	completed := false
	defer func() {
		if completed {
			return
		}
		_ = s.rconAllowTimeout("kickbots")
		_, _ = s.waitForBotRoster(0, nil, 5*time.Second)
	}()

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
			expected = append(expected, name)
			if err := s.addAndConfirmBot(name, entry.team, p.BaseSkill, expected); err != nil {
				return BotCounts{}, err
			}
		}
	}
	st, err := s.waitForBotRoster(2*p.BotsPerTeam, expected, 5*time.Second)
	if err != nil {
		return BotCounts{}, fmt.Errorf("Quake did not report exactly %d named bots after rebuild: %w", 2*p.BotsPerTeam, err)
	}
	completed = true
	// addbot's third argument assigned each exact named bot to red or blue.
	// The stock status protocol cannot read that server-side team field back.
	return BotCounts{TargetPerTeam: p.BotsPerTeam, Red: p.BotsPerTeam, Blue: p.BotsPerTeam, Total: botCounts(st.Players, p.BotsPerTeam).Total, TeamsKnown: false}, nil
}

// addAndConfirmBot retries only an indeterminate addbot. Stock ioquake3 has
// no four-bot ceiling: it either adds the named bot immediately or prints a
// concrete reason (for example, exhausted client slots). RCON runs over UDP,
// however, so a missing reply/roster after one bounded observation may be a
// dropped command. Never send the next bot until this one is observed.
func (s *server) addAndConfirmBot(name, team string, skill int, expected []string) error {
	command := fmt.Sprintf("addbot %s %d %s", safeToken(name), skill, team)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		reply, err := s.rcon(command)
		if err != nil && !isTimeout(err) {
			return fmt.Errorf("could not add bot %q on %s: %w", name, team, err)
		}
		if message := botCommandFailure(reply); message != "" {
			return fmt.Errorf("Quake rejected bot %q on %s: %s", name, team, message)
		}
		if _, err := s.waitForBotRoster(len(expected), expected, 5*time.Second); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// Keep retry traffic well clear of ioquake3's connectionless-packet
		// limiter, rather than immediately compounding an indeterminate add.
		time.Sleep(time.Second)
	}
	return fmt.Errorf("Quake did not add bot %q on %s after 3 attempts: %w", name, team, lastErr)
}

func botCommandFailure(reply string) string {
	clean := strings.TrimSpace(stripQ3Colors(reply))
	lower := strings.ToLower(clean)
	if strings.Contains(lower, "unable to add bot") || strings.Contains(lower, "not defined") || strings.Contains(lower, "error:") {
		return clean
	}
	return ""
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
	if !s.beginBotOperation() {
		http.Error(w, "another bot operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
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

var nextMapSlotPattern = regexp.MustCompile(`(?i)\bvstr\s+d(\d+)\b`)

// nextMap executes the currently installed nextmap chain. It refuses to guess
// at a target: the chain must point at a known saved rotation entry and the
// requested entry must become the live map/mode before this reports success.
func (s *server) nextMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.beginBotOperation() {
		http.Error(w, "another bot or gameplay operation is already running", http.StatusConflict)
		return
	}
	defer s.endBotOperation()
	s.mu.RLock()
	rotation := append(q3.Rotation(nil), s.state.Rotation...)
	s.mu.RUnlock()
	verify, err := s.rcon("nextmap")
	if err != nil {
		http.Error(w, "could not read installed next-map chain: "+err.Error(), http.StatusBadGateway)
		return
	}
	match := nextMapSlotPattern.FindStringSubmatch(verify)
	if len(match) != 2 {
		http.Error(w, "no q3ctl rotation is installed; use Apply at next map first", http.StatusConflict)
		return
	}
	slot, _ := strconv.Atoi(match[1])
	if slot < 1 || slot > len(rotation) {
		http.Error(w, "installed next-map chain does not match the saved rotation; use Apply at next map first", http.StatusConflict)
		return
	}
	want := rotation[slot-1]
	if _, err = s.rcon("vstr nextmap"); err != nil && !isTimeout(err) {
		s.record("next_map", fmt.Sprintf("entry=%d map=%s", slot, want.Map), err.Error())
		http.Error(w, "could not advance to next map: "+err.Error(), http.StatusBadGateway)
		return
	}
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(time.Second)
		live, liveErr := s.live()
		if liveErr == nil && live.Map == want.Map && live.GameType == want.GameType {
			s.record("next_map", fmt.Sprintf("entry=%d map=%s", slot, want.Map), "confirmed")
			out(w, map[string]any{"ok": true, "map": live.Map, "gametype": live.GameType})
			return
		}
	}
	detail := fmt.Sprintf("entry=%d map=%s type=%d was not observed within 30 seconds", slot, want.Map, want.GameType)
	s.record("next_map", detail, "unconfirmed")
	http.Error(w, detail, http.StatusBadGateway)
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

//go:embed dashboard.html
var html string
