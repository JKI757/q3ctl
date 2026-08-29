// q3ctl is a loopback-only Quake 3 control plane. It keeps UDP RCON private
// and exposes a constrained authenticated HTTP UI through Tailscale Serve.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Listen        string `json:"listen"`
	RCONAddr      string `json:"rcon_addr"`
	AdminUser     string `json:"admin_user"`
	StateFile     string `json:"state_file"`
	AuditFile     string `json:"audit_file"`
	AdminPassword string `json:"-"`
	RCONPassword  string `json:"-"`
}
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
type RotationEntry struct {
	Map          string `json:"map"`
	GameType     int    `json:"gametype"`
	TimeLimit    int    `json:"timelimit"`
	FragLimit    int    `json:"fraglimit"`
	CaptureLimit int    `json:"capturelimit"`
}
type Player struct {
	ID      int    `json:"id"`
	Score   int    `json:"score"`
	Ping    int    `json:"ping"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Bot     bool   `json:"bot"`
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
type Persisted struct {
	Policy   BotPolicy       `json:"policy"`
	Rotation []RotationEntry `json:"rotation"`
}
type Audit struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
	Result string    `json:"result"`
}
type server struct {
	cfg         Config
	mu          sync.RWMutex
	state       Persisted
	subscribers map[chan Audit]struct{}
	csrfToken   string
}

func defaults() Persisted {
	return Persisted{Policy: BotPolicy{true, true, "red", 4, []string{"Sarge", "Major", "Grunt", "Crash"}, []string{"Hunter", "Xaero", "Bitterman", "TankJr"}, 3, true, 1, 5}, Rotation: []RotationEntry{{"q3ctf1", 4, 20, 0, 8}, {"q3ctf2", 4, 20, 0, 8}, {"q3dm6", 3, 15, 40, 0}, {"q3dm17", 3, 15, 40, 0}}}
}
func main() {
	p := flag.String("config", "/etc/q3ctl/config.json", "JSON config path")
	flag.Parse()
	cfg, err := load(*p)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{cfg: cfg, state: defaults(), subscribers: map[chan Audit]struct{}{}, csrfToken: newCSRFToken()}
	s.loadState()
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
	mux.HandleFunc("/api/v1/announce", s.announce)
	mux.HandleFunc("/api/v1/players/kick", s.kick)
	mux.HandleFunc("/api/v1/audit", s.audit)
	mux.HandleFunc("/api/v1/logs/stream", s.stream)
	mux.HandleFunc("/", s.dashboard)
	log.Printf("q3ctl listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, s.auth(mux)))
}
func load(path string) (Config, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return Config{}, e
	}
	var c Config
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	c.AdminPassword = os.Getenv("Q3CTL_ADMIN_PASSWORD")
	c.RCONPassword = os.Getenv("Q3CTL_RCON_PASSWORD")
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8088"
	}
	if c.RCONAddr == "" {
		c.RCONAddr = "127.0.0.1:27960"
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/q3ctl/state.json"
	}
	if c.AuditFile == "" {
		c.AuditFile = "/var/log/q3ctl/audit.jsonl"
	}
	if c.AdminUser == "" || c.AdminPassword == "" || c.RCONPassword == "" {
		return c, errors.New("admin_user and Q3CTL_ADMIN_PASSWORD/Q3CTL_RCON_PASSWORD required")
	}
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "[::1]:") {
		return c, errors.New("only loopback listeners allowed")
	}
	return c, nil
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
	c, e := net.DialTimeout("udp", s.cfg.RCONAddr, 2*time.Second)
	if e != nil {
		return "", e
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, e = c.Write([]byte("\xff\xff\xff\xffrcon " + s.cfg.RCONPassword + " " + command + "\n")); e != nil {
		return "", e
	}
	b := make([]byte, 16384)
	n, e := c.Read(b)
	if e != nil {
		return "", e
	}
	return strings.TrimPrefix(string(b[:n]), "\xff\xff\xff\xffprint\n"), nil
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
		name := strings.TrimSpace(m[4])
		addr := strings.TrimSpace(m[5])
		st.Players = append(st.Players, Player{id, score, ping, name, addr, strings.Contains(addr, "bot")})
	}
	return st
}
func (s *server) live() (Status, error) {
	raw, e := s.rcon("status")
	if e != nil {
		return Status{}, e
	}
	return parseStatus(raw), nil
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
	out(w, map[string]any{"server": x, "policy": p.Policy, "rotation": p.Rotation})
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
	s.mu.Lock()
	s.state.Policy = p
	s.saveLocked()
	s.mu.Unlock()
	s.record("bot_policy", "updated", "ok")
	out(w, p)
}
func validatePolicy(p BotPolicy) error {
	if p.HumanTeam != "red" && p.HumanTeam != "blue" {
		return errors.New("team")
	}
	if p.BotsPerTeam < 0 || p.BotsPerTeam > 8 {
		return errors.New("count")
	}
	if p.MinSkill < 1 || p.MaxSkill > 5 || p.BaseSkill < p.MinSkill || p.BaseSkill > p.MaxSkill {
		return errors.New("skill")
	}
	return nil
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
	x, e := s.live()
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	humans := 0
	for _, v := range x.Players {
		if !v.Bot {
			humans++
		}
	} // explicit operator action: never forces humans mid-game; removes bots and recreates roster.
	_, e = s.rcon("kickbots")
	if e == nil {
		time.Sleep(200 * time.Millisecond)
		for _, name := range append(append([]string{}, p.FriendlyBots...), p.OpponentBots...) {
			team := "blue"
			if contains(p.FriendlyBots, name) {
				team = p.HumanTeam
			}
			_, e = s.rcon(fmt.Sprintf("addbot %s %d %s", safeToken(name), p.BaseSkill, team))
			if e != nil {
				break
			}
		}
	}
	detail := fmt.Sprintf("reconcile requested, observed humans=%d, %d bots per roster", humans, len(p.FriendlyBots)+len(p.OpponentBots))
	if e != nil {
		s.record("bot_reconcile", detail, e.Error())
		http.Error(w, e.Error(), 502)
		return
	}
	s.record("bot_reconcile", detail, "ok")
	out(w, map[string]any{"ok": true, "note": "Bots were rebuilt on configured teams. Human team placement remains voluntary in stock Q3; use the in-game team menu.", "humans": humans})
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
	out(w, []string{"q3ctf1", "q3ctf2", "q3ctf3", "q3ctf4", "q3dm1", "q3dm6", "q3dm7", "q3dm17", "q3dm18", "q3dm19"})
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
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || !knownMap(in.Map) || (in.GameType != 3 && in.GameType != 4) {
		http.Error(w, "invalid map or gametype", 400)
		return
	}
	_, e := s.rcon(fmt.Sprintf("set g_gametype %d; map %s", in.GameType, in.Map))
	if e != nil {
		http.Error(w, e.Error(), 502)
		return
	}
	s.record("map_load", fmt.Sprintf("%s type=%d", in.Map, in.GameType), "ok")
	out(w, map[string]bool{"ok": true})
}
func knownMap(m string) bool {
	for _, x := range []string{"q3ctf1", "q3ctf2", "q3ctf3", "q3ctf4", "q3dm1", "q3dm6", "q3dm7", "q3dm17", "q3dm18", "q3dm19"} {
		if m == x {
			return true
		}
	}
	return false
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
	var x []RotationEntry
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&x) != nil || len(x) < 1 || len(x) > 12 {
		http.Error(w, "invalid rotation", 400)
		return
	}
	for _, v := range x {
		if !knownMap(v.Map) || (v.GameType != 3 && v.GameType != 4) {
			http.Error(w, "rotation contains invalid map or mode", 400)
			return
		}
	}
	s.mu.Lock()
	s.state.Rotation = x
	s.saveLocked()
	s.mu.Unlock()
	s.record("rotation", "updated stored rotation", "ok")
	out(w, x)
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
	for _, a := range s.readAudit() {
		b, _ := json.Marshal(a)
		fmt.Fprintf(w, "event: audit\ndata: %s\n\n", b)
	}
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case a := <-ch:
			b, _ := json.Marshal(a)
			fmt.Fprintf(w, "event: audit\ndata: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func (s *server) loadState() {
	b, e := os.ReadFile(s.cfg.StateFile)
	if e == nil {
		var x Persisted
		if json.Unmarshal(b, &x) == nil && validatePolicy(x.Policy) == nil && len(x.Rotation) > 0 {
			s.state = x
		}
	}
}
func (s *server) saveLocked() {
	_ = os.MkdirAll(filepath.Dir(s.cfg.StateFile), 0750)
	b, _ := json.MarshalIndent(s.state, "", "  ")
	_ = os.WriteFile(s.cfg.StateFile, b, 0640)
}
func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, strings.ReplaceAll(html, "__CSRF__", s.csrfToken))
}

var html = `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>q3ctl</title><style>:root{color-scheme:dark}body{font:15px system-ui;background:#10151c;color:#e8edf3;margin:0}main{max-width:1200px;margin:auto;padding:22px}h1{margin:0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(330px,1fr));gap:14px;margin:14px 0}.card{background:#18212c;border:1px solid #304050;border-radius:10px;padding:14px}button,select,input{font:inherit;padding:8px;margin:3px}button{background:#2563eb;color:white;border:0;border-radius:6px}button.danger{background:#b91c1c}pre{white-space:pre-wrap;background:#090d12;padding:12px;max-height:280px;overflow:auto;border-radius:6px}.pill{padding:3px 7px;border-radius:12px;background:#28435d}label{display:block;margin:5px 0}</style><main><h1>Quake 3 Control</h1><p>Private Tailscale dashboard · Server-local RCON</p><div class=grid><section class=card><h2>Live server</h2><div id=summary>Loading…</div><button onclick=refresh()>Refresh status</button><button onclick=post('/api/v1/maps/restart',{})>Restart map (5s)</button><h3>Players</h3><div id=players></div></section><section class=card><h2>Map & mode</h2><label>Map <select id=map></select></label><label>Mode <select id=mode><option value=4>CTF</option><option value=3>TDM</option></select></label><button onclick=loadMap()>Load map</button><h3>Announcement</h3><input id=msg maxlength=140 placeholder="Message to all players"><button onclick=announce()>Send</button></section><section class=card><h2>Bot director</h2><label>Human team <select id=team><option>red</option><option>blue</option></select></label><label>Bots/team <input id=bots type=number min=0 max=8></label><label>Base skill <input id=skill type=number min=1 max=5></label><label><input id=adaptive type=checkbox> adaptive next-round policy</label><button onclick=savePolicy()>Save policy</button><button onclick=reconcile()>Rebuild bot teams</button><p><small>Rebuild is explicit: it removes/re-adds bots. Stock Q3 does not let the server forcibly move human clients without a game mod.</small></p></section><section class=card><h2>Rotation</h2><div id=rotation></div><button onclick=saveRotation()>Save rotation</button><p><small>Stored rotation is ready for next configuration apply; changing it does not interrupt the live match.</small></p></section></div><section class=card><h2>Streaming audit log</h2><pre id=log>Connecting…</pre></section></main><script>const csrf='__CSRF__';let state;const $=id=>document.getElementById(id);async function api(u,o={}){let h=new Headers(o.headers||{});if(o.method&&o.method!=='GET'){h.set('X-CSRF-Token',csrf)}let r=await fetch(u,{...o,headers:h});if(!r.ok)throw Error(await r.text());return r.json()}async function post(u,b){try{await api(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});refresh()}catch(e){alert(e)}}function esc(x){return String(x).replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))}async function refresh(){try{state=await api('/api/v1/status');let s=state.server;$('summary').innerHTML='<b>'+esc(s.map)+'</b> · '+(s.gametype===4?'CTF':'TDM')+' · '+s.players.length+'/'+s.max_clients+' players';$('players').innerHTML=s.players.map(p=>'<div><span class=pill>'+p.id+'</span> '+esc(p.name)+' '+(p.bot?'BOT':'HUMAN')+' score '+p.score+' <button class=danger onclick="kick('+p.id+')">Kick</button></div>').join('')||'Nobody connected';$('team').value=state.policy.human_team;$('bots').value=state.policy.bots_per_team;$('skill').value=state.policy.base_skill;$('adaptive').checked=state.policy.adaptive;$('rotation').innerHTML=state.rotation.map((r,i)=>'<div>'+i+': '+r.map+' · '+(r.gametype===4?'CTF':'TDM')+' · '+r.timelimit+' min</div>').join('')}catch(e){$('summary').textContent='Status unavailable: '+e}}async function kick(id){if(confirm('Kick client '+id+'?'))await post('/api/v1/players/kick',{id})}async function loadMap(){if(confirm('Load selected map now?'))await post('/api/v1/maps/load',{map:$('map').value,gametype:+$('mode').value})}async function announce(){await post('/api/v1/announce',{message:$('msg').value});$('msg').value=''}async function savePolicy(){let p={...state.policy,human_team:$('team').value,bots_per_team:+$('bots').value,base_skill:+$('skill').value,adaptive:$('adaptive').checked};try{await api('/api/v1/bots/policy',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});refresh()}catch(e){alert(e)}}async function reconcile(){if(confirm('Kick and rebuild all bots on configured teams?'))await post('/api/v1/bots/reconcile',{})}async function saveRotation(){try{await api('/api/v1/rotation',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(state.rotation)});alert('Rotation saved')}catch(e){alert(e)}}async function init(){let maps=await api('/api/v1/maps');$('map').innerHTML=maps.map(x=>'<option>'+x+'</option>').join('');refresh();let es=new EventSource('/api/v1/logs/stream');es.addEventListener('audit',e=>{$('log').textContent+=JSON.stringify(JSON.parse(e.data))+'\n';$('log').scrollTop=$('log').scrollHeight});es.onerror=()=>{$('log').textContent+='[stream reconnecting]\n'}}init()</script>`

func init() { sort.Strings([]string{}) }
