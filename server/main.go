package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

type apiResponse struct {
	Name        string  `json:"name"`
	Sex         *string `json:"sex"`
	Vocative    *string `json:"vocative"`
	VocativeCyr *string `json:"vocative_cyr"`
	Status      string  `json:"status"`
}

var db *bolt.DB
var nameIndex map[string]string
var sexCache map[string]string
var vocativeCache map[string]string

// Runtime configuration, populated from environment variables in main().
var (
	namesPath   string
	posthogKey  string
	posthogHost string
	rateLimitRPS float64
)

const maxRecentRequests = 100

type ipRateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rate     float64
	burst    float64
}

type visitor struct {
	tokens   float64
	lastSeen time.Time
}

func newIPRateLimiter(rate, burst float64) *ipRateLimiter {
	rl := &ipRateLimiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		burst:    burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *ipRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, ok := rl.visitors[ip]
	if !ok {
		rl.visitors[ip] = &visitor{tokens: rl.burst - 1, lastSeen: now}
		return true
	}

	elapsed := now.Sub(v.lastSeen).Seconds()
	v.tokens += elapsed * rl.rate
	if v.tokens > rl.burst {
		v.tokens = rl.burst
	}
	v.lastSeen = now

	if v.tokens < 1 {
		return false
	}
	v.tokens--
	return true
}

func (rl *ipRateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for ip, v := range rl.visitors {
			if v.lastSeen.Before(cutoff) {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func withRateLimit(rl *ipRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.allow(ip) {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":  "rate_limited",
				"message": fmt.Sprintf("Too many requests. Limit is %.0f requests per second per IP.", rateLimitRPS),
			})
			return
		}
		next(w, r)
	}
}

// getenv returns the value of the environment variable key, or def when unset/empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type logEvent struct {
	Name   string
	Status string
	Time   time.Time
}

var requestLogCh chan logEvent

func main() {
	// Configuration via environment variables (with sensible defaults).
	dbPath := getenv("DB_PATH", "names.db")
	namesPath = getenv("NAMES_PATH", "../vocative.json")
	posthogKey = os.Getenv("POSTHOG_KEY")
	posthogHost = getenv("POSTHOG_HOST", "https://eu.i.posthog.com")
	rateLimitRPS = 25
	if v := os.Getenv("RATE_LIMIT_RPS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			rateLimitRPS = n
		}
	}

	var err error
	db, err = bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		log.Fatalf("failed to open %s: %v", dbPath, err)
	}
	defer db.Close()

	// Load canonical data from the names JSON file into Bolt buckets (replace any existing)
	if err := replaceBucketsFromJSON(db, namesPath); err != nil {
		log.Fatalf("failed to load %s into DB: %v", namesPath, err)
	}

	// Build case-insensitive index of available names
	idx, err := buildNameIndex(db)
	if err != nil {
		log.Fatalf("failed to build index: %v", err)
	}
	nameIndex = idx

	// Build in-memory caches for bucket values
	sexCache, err = buildBucketMap(db, "sex")
	if err != nil {
		log.Fatalf("failed to build sex cache: %v", err)
	}
	vocativeCache, err = buildBucketMap(db, "vocative")
	if err != nil {
		log.Fatalf("failed to build vocative cache: %v", err)
	}

	// Start background request logger for batched writes
	startRequestLogger()

	limiter := newIPRateLimiter(rateLimitRPS, rateLimitRPS)

	http.HandleFunc("/", handleHome)
	http.HandleFunc("/api/usage", withRateLimit(limiter, handleUsage))
	http.HandleFunc("/api/requests", withRateLimit(limiter, handleRequests))
	http.HandleFunc("/api/all", withRateLimit(limiter, handleAll))
	http.HandleFunc("/api/", withRateLimit(limiter, handleAPI))
	// Serve static favicons from ./favicons at /favicons/
	http.Handle("/favicons/", http.StripPrefix("/favicons/", http.FileServer(http.Dir("favicons"))))
	http.Handle("/favicons_dark/", http.StripPrefix("/favicons_dark/", http.FileServer(http.Dir("favicons_dark"))))
	http.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets"))))

	addr := ":" + getenv("PORT", "3009")
	log.Printf("listening on %s (rate limit %.0f req/s per IP)", addr, rateLimitRPS)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := `<!doctype html>
<html lang="sr">
<head>
<meta charset="utf-8">
<title>Deklinacija.com</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Deklinacija API: case-insensitive API for Serbian given names with vocative forms. Free commercial use.">
<meta name="author" content="Ognjen Jovanović">
<meta name="keywords" content="deklinacija, vokativ, srpska imena, srpski jezik, imena, API, Serbian names, vocative, declension">
<link rel="canonical" href="https://deklinacija.com/">
<meta name="robots" content="index,follow">
<meta name="publisher" content="Ognjen Jovanović">
<link rel="apple-touch-icon" sizes="180x180" href="/favicons/apple-touch-icon.png" media="(prefers-color-scheme: light)">
<link rel="apple-touch-icon" sizes="180x180" href="/favicons_dark/apple-touch-icon.png" media="(prefers-color-scheme: dark)">
<link id="fav32" rel="icon" type="image/png" sizes="32x32" href="/favicons/favicon-32x32.png" media="(prefers-color-scheme: light)">
<link id="fav32d" rel="icon" type="image/png" sizes="32x32" href="/favicons_dark/favicon-32x32.png" media="(prefers-color-scheme: dark)">
<link id="fav16" rel="icon" type="image/png" sizes="16x16" href="/favicons/favicon-16x16.png" media="(prefers-color-scheme: light)">
<link id="fav16d" rel="icon" type="image/png" sizes="16x16" href="/favicons_dark/favicon-16x16.png" media="(prefers-color-scheme: dark)">
<link rel="manifest" href="/favicons/site.webmanifest">
<link rel="mask-icon" href="/favicons/safari-pinned-tab.svg" color="#5bbad5">
<link id="favico" rel="shortcut icon" href="/favicons/favicon.ico" media="(prefers-color-scheme: light)">
<link id="favicod" rel="shortcut icon" href="/favicons_dark/favicon.ico" media="(prefers-color-scheme: dark)">
<meta name="msapplication-TileColor" content="#2b5797">
<meta name="msapplication-config" content="/favicons/browserconfig.xml">
<meta name="theme-color" content="#ffffff" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#0d1117" media="(prefers-color-scheme: dark)">
<script>(function(){try{var t=localStorage.getItem('theme');if(t==='light'||t==='dark'){document.documentElement.setAttribute('data-theme',t);}}catch(e){}})();</script>
<style>
:root,:root[data-theme="light"]{
  --bg:#ffffff;--canvas:#f6f8fa;--surface:#ffffff;--surface-2:#f6f8fa;
  --text:#1f2328;--muted:#656d76;--border:#d0d7de;--border-muted:#d8dee4;
  --accent:#0969da;--accent-fg:#ffffff;--accent-emphasis:#0860ca;
  --success-bg:#dafbe1;--success-fg:#1a7f37;--success-border:#1a7f3733;
  --danger-bg:#ffebe9;--danger-fg:#cf222e;--danger-border:#cf222e33;
  --shadow:0 1px 0 rgba(31,35,40,.04),0 1px 3px rgba(31,35,40,.06);
  --mono:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;
}
:root[data-theme="dark"]{
  --bg:#0d1117;--canvas:#010409;--surface:#161b22;--surface-2:#0d1117;
  --text:#e6edf3;--muted:#8b949e;--border:#30363d;--border-muted:#21262d;
  --accent:#2f81f7;--accent-fg:#ffffff;--accent-emphasis:#388bfd;
  --success-bg:#12261e;--success-fg:#3fb950;--success-border:#3fb95044;
  --danger-bg:#25171c;--danger-fg:#f85149;--danger-border:#f8514944;
  --shadow:0 0 transparent;
}
@media (prefers-color-scheme: dark){
  :root:not([data-theme="light"]){
    --bg:#0d1117;--canvas:#010409;--surface:#161b22;--surface-2:#0d1117;
    --text:#e6edf3;--muted:#8b949e;--border:#30363d;--border-muted:#21262d;
    --accent:#2f81f7;--accent-fg:#ffffff;--accent-emphasis:#388bfd;
    --success-bg:#12261e;--success-fg:#3fb950;--success-border:#3fb95044;
    --danger-bg:#25171c;--danger-fg:#f85149;--danger-border:#f8514944;
    --shadow:0 0 transparent;
  }
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans",Helvetica,Arial,sans-serif;margin:0;line-height:1.5;color:var(--text);background:var(--canvas);font-size:16px}
.wrap{max-width:1012px;margin:0 auto;padding:0 16px}
header.site{position:sticky;top:0;z-index:10;background:var(--surface);border-bottom:1px solid var(--border);backdrop-filter:saturate(180%) blur(6px)}
header.site .wrap{display:flex;align-items:center;gap:12px;height:60px}
.brand{display:flex;align-items:center;gap:10px;font-weight:600;font-size:16px;color:var(--text);text-decoration:none}
.brand img{display:block;border-radius:4px}
.brand .tag{font-family:var(--mono);font-size:12px;color:var(--muted);font-weight:500;border:1px solid var(--border);padding:1px 6px;border-radius:999px}
.spacer{flex:1}
.ghbtn{display:inline-flex;align-items:center;gap:6px;font-size:14px;font-weight:600;color:var(--text);background:var(--surface-2);border:1px solid var(--border);border-radius:6px;padding:6px 12px;text-decoration:none;transition:background .15s}
.ghbtn:hover{background:var(--border-muted)}
.themeBtn{display:inline-flex;align-items:center;justify-content:center;width:36px;height:36px;color:var(--text);background:var(--surface-2);border:1px solid var(--border);border-radius:6px;padding:0;cursor:pointer;transition:background .15s}
.themeBtn:hover{background:var(--border-muted)}
.themeBtn .ic-sun{display:none}
.themeBtn .ic-moon{display:block}
:root[data-theme="dark"] .themeBtn .ic-sun{display:block}
:root[data-theme="dark"] .themeBtn .ic-moon{display:none}
@media (prefers-color-scheme: dark){
  :root:not([data-theme="light"]) .themeBtn .ic-sun{display:block}
  :root:not([data-theme="light"]) .themeBtn .ic-moon{display:none}
}
.hero{padding:40px 0 8px}
.hero h1{font-size:clamp(28px,5vw,40px);line-height:1.15;margin:0 0 12px;letter-spacing:-.5px}
.hero p.lead{font-size:18px;color:var(--muted);margin:0 0 20px;max-width:680px}
.pills{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:8px}
.pill{font-size:12px;font-weight:500;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:999px;padding:4px 10px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:20px;margin:20px 0;box-shadow:var(--shadow)}
.card h2{font-size:16px;margin:0 0 14px;display:flex;align-items:center;gap:8px;font-weight:600}
.card h2 .ic{color:var(--muted)}
.muted{color:var(--muted)}
small{color:var(--muted);font-size:12px}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}
code{font-family:var(--mono);font-size:85%;background:rgba(127,127,127,.16);color:var(--text);padding:.2em .4em;border-radius:6px}
pre{font-family:var(--mono);font-size:13px;background:var(--surface-2);color:var(--text);border:1px solid var(--border);padding:14px 16px;border-radius:10px;overflow:auto;line-height:1.45;white-space:pre-wrap;word-break:break-word;margin:0 0 12px}
pre code{background:none;padding:0;font-size:inherit}
.inputRow{display:flex;align-items:center;gap:8px;margin:6px 0 14px;padding:6px;border:1px solid var(--border);border-radius:10px;background:var(--surface-2)}
.inputRow .prefix{font-family:var(--mono);font-size:13px;color:var(--muted);padding:0 4px 0 8px;user-select:none}
.inputRow input{flex:1;border:1px solid transparent;border-radius:8px;padding:9px 10px;font-size:15px;background:var(--surface);color:var(--text)}
.inputRow input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 25%,transparent)}
.inputRow button{display:inline-flex;align-items:center;gap:6px;background:var(--accent);color:var(--accent-fg);border:1px solid color-mix(in srgb,var(--accent) 80%,#000);border-radius:8px;padding:9px 16px;font-weight:600;font-size:14px;cursor:pointer;transition:background .15s}
.inputRow button:hover{background:var(--accent-emphasis)}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin:20px 0}
.grid .card{margin:0;height:100%}
.list{height:180px;overflow:auto}
.listItem{display:flex;gap:10px;align-items:center;padding:7px 2px;border-bottom:1px solid var(--border-muted);font-size:14px}
.listItem:last-child{border-bottom:0}
.badge{font-size:11px;font-weight:600;border-radius:999px;padding:2px 9px;border:1px solid transparent;white-space:nowrap}
.badge.s{background:var(--success-bg);color:var(--success-fg);border-color:var(--success-border)}
.badge.n{background:var(--danger-bg);color:var(--danger-fg);border-color:var(--danger-border)}
.badge.f{background:var(--danger-bg);color:var(--danger-fg);border-color:var(--danger-border)}
dialog#nonoDialog{border:1px solid var(--border);border-radius:12px;padding:0;background:var(--surface);color:var(--text);box-shadow:0 8px 32px rgba(0,0,0,.24);max-width:min(360px,92vw)}
dialog#nonoDialog::backdrop{background:rgba(0,0,0,.45)}
dialog#nonoDialog .nonoBody{padding:16px;display:flex;flex-direction:column;align-items:center;gap:14px}
dialog#nonoDialog img{display:block;width:100%;max-width:320px;height:auto;border-radius:8px}
dialog#nonoDialog button{display:inline-flex;align-items:center;justify-content:center;background:var(--accent);color:var(--accent-fg);border:1px solid color-mix(in srgb,var(--accent) 80%,#000);border-radius:8px;padding:8px 20px;font-weight:600;font-size:14px;cursor:pointer}
dialog#nonoDialog button:hover{background:var(--accent-emphasis)}
.legend{display:flex;gap:14px;flex-wrap:wrap;margin-top:10px}
.legend span{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:var(--muted)}
.dot{width:9px;height:9px;border-radius:999px;display:inline-block}
footer.site{border-top:1px solid var(--border);margin-top:40px;padding:24px 0;color:var(--muted);font-size:14px}
footer.site .wrap{display:flex;flex-wrap:wrap;gap:8px;align-items:center;justify-content:space-between}
@media (max-width:820px){.grid{grid-template-columns:1fr}}
@media (max-width:520px){
  .hero{padding:28px 0 4px}
  .card{padding:16px}
  header.site .brand .tag{display:none}
  .inputRow{flex-wrap:wrap}
  .inputRow input{min-width:0}
  .inputRow button{flex:1;justify-content:center}
}
</style>
<script>
async function drawUsage(){
  try{
    const res = await fetch('/api/usage',{cache:'no-store'});
    const data = await res.json();
    const labels = data.map(p=>p.minute.slice(8,10)+":"+p.minute.slice(10,12));
    const totals = data.map(p=>p.total||p.Total||0);
    const ok = data.map(p=>p.success||p.Success||0);
    const nf = data.map(p=>p.notFound||p.NotFound||0);
    const ctx = document.getElementById('usage');
    if(!ctx) return;
    const w = ctx.width = ctx.clientWidth; const h = ctx.height = 160;
    const c = ctx.getContext('2d');
    c.clearRect(0,0,w,h);
    const cs = getComputedStyle(document.documentElement);
    const col = (n,fallback)=>{ const v=cs.getPropertyValue(n).trim(); return v||fallback; };
    const gridCol = col('--border-muted','#d8dee4');
    const accentCol = col('--accent','#0969da');
    const okCol = col('--success-fg','#1a7f37');
    const nfCol = col('--danger-fg','#cf222e');
    const maxY = Math.max(1, ...totals);
    const padL=30,padR=10,padT=12,padB=20; const innerW=w-padL-padR, innerH=h-padT-padB;
    function x(i){ return padL + i * (innerW/Math.max(1, totals.length-1)); }
    function y(v){ return padT + innerH - (v/maxY)*innerH; }
    const steps = Math.min(4, maxY);
    c.strokeStyle = gridCol; c.lineWidth=1; c.beginPath();
    for(let s=0; s<=steps; s++){
      const yy = padT + innerH - (s/steps)*innerH;
      c.moveTo(padL,yy); c.lineTo(w-padR,yy);
    }
    c.stroke();
    function line(arr,color,width){
      c.strokeStyle=color; c.lineWidth=width; c.lineJoin='round'; c.beginPath();
      arr.forEach((v,i)=>{ const xx=x(i), yy=y(v); if(i===0)c.moveTo(xx,yy); else c.lineTo(xx,yy); });
      c.stroke();
    }
    line(totals, accentCol, 2);
    line(ok, okCol, 1.5);
    line(nf, nfCol, 1.5);
  }catch(e){/* ignore */}
}
async function drawRequests(){
  try{
    const res = await fetch('/api/requests',{cache:'no-store'});
    const data = await res.json();
    const box = document.getElementById('reqList');
    if(!box) return;
    const fmt = (t)=>{
      // Expect RFC3339Nano; show HH:MM:SS
      const d = new Date(t);
      if(isNaN(d)) return t;
      const hh = String(d.getHours()).padStart(2,'0');
      const mm = String(d.getMinutes()).padStart(2,'0');
      const ss = String(d.getSeconds()).padStart(2,'0');
      return hh+':'+mm+':'+ss;
    };
    box.innerHTML = '';
    if(!data || !data.length){ box.innerHTML = '<div class="muted" style="padding:8px 2px;font-size:14px">No requests in the last hour.</div>'; return; }
    data.slice(0, 100).forEach((r)=>{
      const status = (r.status||r.Status||'');
      const good = /^Success$/i.test(status);
      const failed = /^failed$/i.test(status);
      const badge = good? '<span class="badge s">Success</span>' : (failed? '<span class="badge f">failed</span>' : '<span class="badge n">Not found</span>');
      const time = fmt(r.time||r.Time||'');
      const name = r.name||r.Name||'';
      const row = document.createElement('div');
      row.className='listItem';
      row.innerHTML = badge + '<span class="muted" style="width:64px;font-variant-numeric:tabular-nums">' + time + '</span><span style="font-weight:600">' + name + '</span>';
      box.appendChild(row);
    });
  }catch(e){/* ignore */}
}
function looksLikeHTML(s){
  return /[<>]/.test(s||'');
}
function showNonoDialog(){
  const dlg = document.getElementById('nonoDialog');
  if(dlg && typeof dlg.showModal==='function') dlg.showModal();
}
async function queryAPI(){
  const input = document.getElementById('nameInput');
  const out = document.getElementById('result');
  if(!input || !out) return;
  const name = (input.value||'').trim();
  if(!name){ out.textContent=''; return; }
  const htmlAttempt = looksLikeHTML(name);
  out.textContent = 'Loading...';
  try{
    const res = await fetch('/api/'+encodeURIComponent(name), {cache:'no-store'});
    const text = await res.text();
    try{ out.textContent = JSON.stringify(JSON.parse(text), null, 2); }
    catch(_){ out.textContent = text; }
    if(htmlAttempt) showNonoDialog();
  }catch(e){ out.textContent = 'Network error'; }
}
function currentTheme(){
  const explicit = document.documentElement.getAttribute('data-theme');
  if(explicit==='light'||explicit==='dark') return explicit;
  return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}
// Favicon <link> media attrs only follow the OS setting, so when an explicit
// theme is chosen we override media to force the matching icon set; passing
// null restores OS-based behavior (prefers-color-scheme).
function applyFavicons(theme){
  const pairs = [['fav32','fav32d'],['fav16','fav16d'],['favico','favicod']];
  pairs.forEach(([lightId,darkId])=>{
    const l = document.getElementById(lightId);
    const d = document.getElementById(darkId);
    if(!l||!d) return;
    if(theme==='dark'){ l.media='not all'; d.media='all'; }
    else if(theme==='light'){ l.media='all'; d.media='not all'; }
    else { l.media='(prefers-color-scheme: light)'; d.media='(prefers-color-scheme: dark)'; }
  });
  const brand = document.getElementById('brandLogo');
  if(brand){
    const eff = (theme==='light'||theme==='dark') ? theme : currentTheme();
    brand.src = (eff==='dark' ? '/favicons_dark' : '/favicons') + '/favicon-32x32.png';
  }
}
function toggleTheme(){
  const next = currentTheme()==='dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  try{ localStorage.setItem('theme', next); }catch(e){}
  applyFavicons(next);
  drawUsage();
}
addEventListener('load',()=>{
  const themeBtn = document.getElementById('themeBtn');
  if(themeBtn){ themeBtn.addEventListener('click', toggleTheme); }
  let stored=null; try{ stored=localStorage.getItem('theme'); }catch(e){}
  applyFavicons(stored==='light'||stored==='dark' ? stored : null);
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change',()=>{
    let s=null; try{ s=localStorage.getItem('theme'); }catch(e){}
    if(s!=='light'&&s!=='dark'){ applyFavicons(null); drawUsage(); }
  });
  drawUsage();
  setInterval(drawUsage, 10000);
  drawRequests();
  setInterval(drawRequests, 10000);
  const input = document.getElementById('nameInput');
  const btn = document.getElementById('sendBtn');
  if(input){ input.addEventListener('keydown', e=>{ if(e.key==='Enter'){ queryAPI(); } }); }
  if(btn){ btn.addEventListener('click', queryAPI); }
  if(input){ input.value = 'Ognjen'; }
  queryAPI();
});
</script>
__ANALYTICS__
</head>
<body>
<header class="site">
  <div class="wrap">
    <a class="brand" href="/">
      <img id="brandLogo" src="/favicons/favicon-32x32.png" alt="" width="28" height="28">
      <span>Deklinacija.com</span>
    </a>
    <span class="spacer"></span>
    <button id="themeBtn" class="themeBtn" type="button" aria-label="Toggle dark mode" title="Toggle dark mode">
      <svg class="ic-moon" width="18" height="18" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M9.598 1.591a.749.749 0 0 1 .785-.175 7.001 7.001 0 1 1-8.967 8.967.75.75 0 0 1 .961-.96 5.5 5.5 0 0 0 7.046-7.046.75.75 0 0 1 .175-.786Zm1.616 1.945a7 7 0 0 1-7.678 7.678 5.499 5.499 0 1 0 7.678-7.678Z"/></svg>
      <svg class="ic-sun" width="18" height="18" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 12a4 4 0 1 1 0-8 4 4 0 0 1 0 8Zm0-1.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5Zm5.657-8.157a.75.75 0 0 1 0 1.061l-1.061 1.06a.749.749 0 0 1-1.275-.326.749.749 0 0 1 .215-.734l1.06-1.06a.75.75 0 0 1 1.06 0Zm-9.193 9.193a.75.75 0 0 1 0 1.06l-1.06 1.061a.75.75 0 1 1-1.061-1.06l1.06-1.061a.75.75 0 0 1 1.061 0ZM8 0a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0V.75A.75.75 0 0 1 8 0ZM3 8a.75.75 0 0 1-.75.75H.75a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 3 8Zm13 0a.75.75 0 0 1-.75.75h-1.5a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 16 8Zm-8 5a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 13Zm3.536-1.464a.75.75 0 0 1 1.06 0l1.061 1.06a.75.75 0 0 1-1.06 1.061l-1.061-1.06a.75.75 0 0 1 0-1.061ZM2.343 2.343a.75.75 0 0 1 1.061 0l1.06 1.061a.751.751 0 0 1-.018 1.042.751.751 0 0 1-1.042.018l-1.06-1.06a.75.75 0 0 1 0-1.061Z"/></svg>
    </button>
    <a class="ghbtn" href="https://github.com/ogi-joo/deklinacija.com" target="_blank" rel="noopener">
      <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 0a8 8 0 0 0-2.53 15.59c.4.07.55-.17.55-.38v-1.33c-2.23.49-2.7-1.07-2.7-1.07-.36-.93-.89-1.18-.89-1.18-.73-.5.05-.49.05-.49.8.06 1.23.83 1.23.83.71 1.23 1.87.87 2.33.67.07-.52.28-.87.5-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.83-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.22 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.52.56.83 1.28.83 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48v2.2c0 .21.15.46.55.38A8 8 0 0 0 8 0Z"/></svg>
      Star on GitHub
    </a>
  </div>
</header>

<main class="wrap">
  <section class="hero">
    <h1>Balkan names with vocative and sex.</h1>
    <p class="lead">A fast, case-insensitive API that resolves Balkan given names to their <strong>sex</strong> and <strong>vocative</strong> form — in both Latin and Cyrillic. Free for commercial use.</p>
    <div class="pills">
      <span class="pill">No API key</span>
      <span class="pill">25 req/s per IP</span>
      <span class="pill">Cyrillic &amp; Latin</span>
      <span class="pill">Open source</span>
    </div>
  </section>

  <div class="card">
    <h2><span class="ic">&#9889;</span> Try it</h2>
    <div class="inputRow">
      <span class="prefix">GET /api/</span>
      <input id="nameInput" type="text" placeholder="Type a name…" autocomplete="off" autocapitalize="words" inputmode="text" enterkeyhint="go" />
      <button id="sendBtn">Send</button>
    </div>
    <pre id="result"><code></code></pre>
    <small>Try <code>Ognjen</code>, <code>Огњен</code>, or <code>Milica</code>. Lookups are case-insensitive.</small>
  </div>

  <div class="grid">
    <div class="card">
      <h2><span class="ic">&#128225;</span> Quickstart</h2>
      <p class="muted" style="margin-top:0">HTTP endpoint:</p>
      <pre><code>curl https://deklinacija.com/api/Ognjen</code></pre>
      <p class="muted">NPM package:</p>
      <pre><code>npm install deklinacija</code></pre>
      <pre><code>import dekl from 'deklinacija'
dekl("Ognjen").vocative     // "Ognjene"
dekl("Ognjen").vocativeCyr  // "Огњене"
dekl("Ognjen").sex          // "male"</code></pre>
    </div>
    <div class="card">
      <h2><span class="ic">&#123;&#125;</span> Response format</h2>
      <pre><code>{
  "name": string,
  "sex": "male" | "female" | "both" | null,
  "vocative": string | null,
  "vocative_cyr": string | null,
  "status": "Success" | "Not found" | "failed"
}</code></pre>
      <p class="muted">Example:</p>
      <pre><code>{
  "name": "Ognjen",
  "sex": "male",
  "vocative": "Ognjene",
  "vocative_cyr": "Огњене",
  "status": "Success"
}</code></pre>
    </div>
  </div>

  <div class="grid">
    <div class="card">
      <h2><span class="ic">&#128200;</span> Live usage</h2>
      <canvas id="usage" style="width:100%;height:160px"></canvas>
      <div class="legend">
        <span><i class="dot" style="background:var(--accent)"></i> Total</span>
        <span><i class="dot" style="background:var(--success-fg)"></i> Success</span>
        <span><i class="dot" style="background:var(--danger-fg)"></i> Not found</span>
        <span class="muted">· last 60 min · auto-refresh 10s</span>
      </div>
    </div>
    <div class="card">
      <h2><span class="ic">&#128172;</span> Recent requests</h2>
      <div id="reqList" class="list"></div>
    </div>
  </div>
</main>

<footer class="site">
  <div class="wrap">
    <span>Created by <strong>Ognjen Jovanović</strong></span>
    <span><a href="https://github.com/ogi-joo" target="_blank" rel="noopener">github.com/ogi-joo</a> · MIT licensed</span>
  </div>
</footer>
<dialog id="nonoDialog">
  <form method="dialog" class="nonoBody">
    <img src="/assets/nono.jpeg" alt="No no no" width="320" height="320">
    <button value="ok">OK</button>
  </form>
</dialog>
</body>
</html>`
	page = strings.Replace(page, "__ANALYTICS__", analyticsSnippet(), 1)
	fmt.Fprint(w, page)
}

// analyticsSnippet returns the PostHog bootstrap script when POSTHOG_KEY is configured,
// or an empty string otherwise. This keeps analytics opt-in for self-hosted deployments.
func analyticsSnippet() string {
	if posthogKey == "" {
		return ""
	}
	host := posthogHost
	if host == "" {
		host = "https://eu.i.posthog.com"
	}
	return `<script>
    !function(t,e){var o,n,p,r;e.__SV||(window.posthog=e,e._i=[],e.init=function(i,s,a){function g(t,e){var o=e.split(".");2==o.length&&(t=t[o[0]],e=o[1]),t[e]=function(){t.push([e].concat(Array.prototype.slice.call(arguments,0)))}}(p=t.createElement("script")).type="text/javascript",p.crossOrigin="anonymous",p.async=!0,p.src=s.api_host.replace(".i.posthog.com","-assets.i.posthog.com")+"/static/array.js",(r=t.getElementsByTagName("script")[0]).parentNode.insertBefore(p,r);var u=e;for(void 0!==a?u=e[a]=[]:a="posthog",u.people=u.people||[],u.toString=function(t){var e="posthog";return"posthog"!==a&&(e+="."+a),t||(e+=" (stub)"),e},u.people.toString=function(){return u.toString(1)+".people (stub)"},o="init Ce js Ls Te Fs Ds capture Ye calculateEventProperties zs register register_once register_for_session unregister unregister_for_session Ws getFeatureFlag getFeatureFlagPayload isFeatureEnabled reloadFeatureFlags updateEarlyAccessFeatureEnrollment getEarlyAccessFeatures on onFeatureFlags onSurveysLoaded onSessionId getSurveys getActiveMatchingSurveys renderSurvey displaySurvey canRenderSurvey canRenderSurveyAsync identify setPersonProperties group resetGroups setPersonPropertiesForFlags resetPersonPropertiesForFlags setGroupPropertiesForFlags resetGroupPropertiesForFlags reset get_distinct_id getGroups get_session_id get_session_replay_url alias set_config startSessionRecording stopSessionRecording sessionRecordingStarted captureException loadToolbar get_property getSessionProperty Bs Us createPersonProfile Hs Ms Gs opt_in_capturing opt_out_capturing has_opted_in_capturing has_opted_out_capturing get_explicit_consent_status is_capturing clear_opt_in_out_capturing Ns debug L qs getPageViewId captureTraceFeedback captureTraceMetric".split(" "),n=0;n<o.length;n++)g(u,o[n]);e._i.push([i,s,a])},e.__SV=1)}(document,window.posthog||[]);
    posthog.init('` + posthogKey + `', {
        api_host: '` + host + `',
        defaults: '2025-05-24',
        person_profiles: 'identified_only',
    })
</script>`
}

func isRejectedInput(name string) bool {
	return strings.ContainsAny(name, "<>")
}

func writeFailedNoNo(w http.ResponseWriter) {
	_ = recordRequest("No no...", "failed")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Name:        "No no...",
		Sex:         nil,
		Vocative:    nil,
		VocativeCyr: nil,
		Status:      "failed",
	})
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	// Expect path like /api/:name
	namePart := strings.TrimPrefix(r.URL.Path, "/api/")
	if namePart == "" || namePart == "/" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	// Support URL-encoded names, preserve original casing and diacritics
	decoded, err := url.PathUnescape(namePart)
	if err != nil || !utf8.ValidString(decoded) {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	name := decoded

	if isRejectedInput(name) {
		writeFailedNoNo(w)
		return
	}

	// If input contains Cyrillic, transliterate to Latin for lookup
	query := name
	if containsCyrillic(query) {
		query = toLatinSerbian(query)
	}

	// Resolve canonical name using case-insensitive index (on Latin query)
	lowerQuery := strings.ToLower(query)
	canonical, ok := nameIndex[lowerQuery]
	if !ok {
		// Fallback: try Latin digraph approximations (dj→đ, dz→dž)
		if can2, ok2 := findWithDigraphFallback(lowerQuery); ok2 {
			canonical, ok = can2, true
		}
	}
	if !ok {
		_ = recordRequest(name, "Not found")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(apiResponse{Name: name, Sex: nil, Vocative: nil, VocativeCyr: nil, Status: "Not found"})
		return
	}

	sexVal := sexCache[canonical]
	vocVal := vocativeCache[canonical]
	_ = recordRequest(canonical, "Success")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	vocCyr := toCyrillicSerbian(vocVal)
	resp := apiResponse{Name: canonical, Sex: &sexVal, Vocative: &vocVal, VocativeCyr: &vocCyr, Status: "Success"}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
}

// handleUsage returns minute-binned usage for the last 60 minutes
func handleUsage(w http.ResponseWriter, r *http.Request) {
	// Build last 60 minute keys
	now := time.Now().UTC()
	type point struct {
		Minute   string `json:"minute"`
		Total    uint64 `json:"total"`
		Success  uint64 `json:"success"`
		NotFound uint64 `json:"notFound"`
	}
	var series []point
	keys := make([]string, 60)
	for i := 59; i >= 0; i-- {
		keys[59-i] = now.Add(-time.Duration(i) * time.Minute).Format("200601021504")
	}

	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("stats_minute"))
		for _, k := range keys {
			var p point
			p.Minute = k
			if b != nil {
				if v := b.Get([]byte(k)); v != nil {
					var s statsEntry
					if err := json.Unmarshal(v, &s); err == nil {
						p.Total, p.Success, p.NotFound = s.Total, s.Success, s.NotFound
					}
				}
			}
			series = append(series, p)
		}
		return nil
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(series)
}

// handleRequests returns up to 100 requests from the last 60 minutes (most recent first)
func handleRequests(w http.ResponseWriter, r *http.Request) {
	cutoff := time.Now().UTC().Add(-60 * time.Minute)
	type entry struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Time   string `json:"time"`
	}
	var out []entry
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("requests_log"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		// Iterate descending from the end
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if len(out) >= maxRecentRequests {
				break
			}
			// key starts with RFC3339Nano timestamp
			parts := strings.SplitN(string(k), "|", 2)
			if len(parts) == 0 {
				continue
			}
			tstr := parts[0]
			t, err := time.Parse(time.RFC3339Nano, tstr)
			if err != nil {
				continue
			}
			if t.Before(cutoff) {
				break
			}
			var e entry
			if err := json.Unmarshal(v, &e); err == nil {
				out = append(out, e)
			}
		}
		return nil
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

// handleAll returns all names data as JSON
func handleAll(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(namesPath)
	if err != nil {
		http.Error(w, "failed to read names data", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}

func readBucketValue(bucket, key string) (string, error) {
	var out string
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v != nil {
			out = string(v)
		}
		return nil
	})
	return out, err
}

// statsEntry stores per-minute counters
type statsEntry struct {
	Total    uint64 `json:"t"`
	Success  uint64 `json:"s"`
	NotFound uint64 `json:"n"`
}

// recordRequest logs a request with timestamp and updates per-minute stats
func recordRequest(name, status string) error {
	// Non-blocking send to background logger; drop on full buffer to avoid latency
	select {
	case requestLogCh <- logEvent{Name: name, Status: status, Time: time.Now().UTC()}:
	default:
		// drop
	}
	return nil
}

// buildNameIndex scans DB buckets and builds a lowercase -> canonical name map
func buildNameIndex(db *bolt.DB) (map[string]string, error) {
	idx := make(map[string]string, 2048)
	err := db.View(func(tx *bolt.Tx) error {
		merge := func(b *bolt.Bucket) {
			if b == nil {
				return
			}
			c := b.Cursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				name := string(k)
				lower := strings.ToLower(name)
				if _, exists := idx[lower]; !exists {
					idx[lower] = name
				}
			}
		}
		merge(tx.Bucket([]byte("sex")))
		merge(tx.Bucket([]byte("vocative")))
		return nil
	})
	return idx, err
}

// replaceBucketsFromJSON loads names from JSON file and replaces BoltDB buckets `sex` and `vocative`
func replaceBucketsFromJSON(db *bolt.DB, path string) error {
	type rec struct {
		Name     string `json:"name"`
		Sex      string `json:"sex"`
		Vocative string `json:"vocative"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var records []rec
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	// Replace buckets inside a single write transaction
	return db.Update(func(tx *bolt.Tx) error {
		// Drop old buckets if they exist
		_ = tx.DeleteBucket([]byte("sex"))
		_ = tx.DeleteBucket([]byte("vocative"))
		// Recreate buckets
		sexB, err := tx.CreateBucket([]byte("sex"))
		if err != nil {
			return err
		}
		vocB, err := tx.CreateBucket([]byte("vocative"))
		if err != nil {
			return err
		}
		for _, r := range records {
			if r.Name == "" {
				continue
			}
			if err := sexB.Put([]byte(r.Name), []byte(r.Sex)); err != nil {
				return err
			}
			if err := vocB.Put([]byte(r.Name), []byte(r.Vocative)); err != nil {
				return err
			}
		}
		return nil
	})
}

// buildBucketMap loads all key->value pairs from a bucket into an in-memory map keyed by canonical name
func buildBucketMap(db *bolt.DB, bucketName string) (map[string]string, error) {
	out := make(map[string]string, 2048)
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			out[string(k)] = string(v)
		}
		return nil
	})
	return out, err
}

// startRequestLogger launches a background goroutine that batches request logs and stats updates
func startRequestLogger() {
	if requestLogCh == nil {
		requestLogCh = make(chan logEvent, 4096)
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		buffer := make([]logEvent, 0, 512)
		flush := func(events []logEvent) {
			if len(events) == 0 {
				return
			}
			// Aggregate per-minute stats
			type aggEntry struct{ Total, Success, NotFound uint64 }
			statsAgg := make(map[string]aggEntry)
			_ = db.Update(func(tx *bolt.Tx) error {
				logsB, err := tx.CreateBucketIfNotExists([]byte("requests_log"))
				if err != nil {
					return err
				}
				statsB, err := tx.CreateBucketIfNotExists([]byte("stats_minute"))
				if err != nil {
					return err
				}
				// Append all logs and build stats aggregation
				for _, ev := range events {
					seq, _ := logsB.NextSequence()
					logKey := fmt.Sprintf("%s|%012d", ev.Time.Format(time.RFC3339Nano), seq)
					entry := struct {
						Name   string `json:"name"`
						Status string `json:"status"`
						Time   string `json:"time"`
					}{Name: ev.Name, Status: ev.Status, Time: ev.Time.Format(time.RFC3339Nano)}
					buf, _ := json.Marshal(entry)
					if err := logsB.Put([]byte(logKey), buf); err != nil {
						return err
					}
					mk := ev.Time.UTC().Format("200601021504")
					s := statsAgg[mk]
					s.Total++
					if ev.Status == "Success" {
						s.Success++
					} else {
						s.NotFound++
					}
					statsAgg[mk] = s
				}
				// Apply aggregated stats
				for mk, delta := range statsAgg {
					var s statsEntry
					if v := statsB.Get([]byte(mk)); v != nil {
						_ = json.Unmarshal(v, &s)
					}
					s.Total += delta.Total
					s.Success += delta.Success
					s.NotFound += delta.NotFound
					newV, _ := json.Marshal(s)
					if err := statsB.Put([]byte(mk), newV); err != nil {
						return err
					}
				}
				return nil
			})
		}
		for {
			select {
			case ev := <-requestLogCh:
				buffer = append(buffer, ev)
				if len(buffer) >= 512 {
					flush(buffer)
					buffer = buffer[:0]
				}
			case <-ticker.C:
				if len(buffer) > 0 {
					flush(buffer)
					buffer = buffer[:0]
				}
			}
		}
	}()
}

// findWithDigraphFallback tries alternative Latin spellings commonly used when users lack diacritics.
// It expects a lower-cased query.
func findWithDigraphFallback(lower string) (string, bool) {
	if lower == "" {
		return "", false
	}
	// Generate candidates
	// 1) Replace dj -> đ
	cand1 := strings.ReplaceAll(lower, "dj", "đ")
	if can, ok := nameIndex[cand1]; ok {
		return can, true
	}
	// 2) Replace dz -> dž
	cand2 := strings.ReplaceAll(lower, "dz", "dž")
	if can, ok := nameIndex[cand2]; ok {
		return can, true
	}
	// 3) Replace both dj and dz in one pass
	repl := strings.NewReplacer("dj", "đ", "dz", "dž")
	cand3 := repl.Replace(lower)
	if can, ok := nameIndex[cand3]; ok {
		return can, true
	}
	return "", false
}

// containsCyrillic reports whether the string contains any Cyrillic script runes
func containsCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// toLatinSerbian transliterates Serbian Cyrillic to Serbian Latin.
// It preserves letter casing and maps digraph letters Љ/Њ/Џ appropriately.
func toLatinSerbian(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		// lowercase
		case 'а':
			b.WriteString("a")
		case 'б':
			b.WriteString("b")
		case 'в':
			b.WriteString("v")
		case 'г':
			b.WriteString("g")
		case 'д':
			b.WriteString("d")
		case 'ђ':
			b.WriteString("đ")
		case 'е':
			b.WriteString("e")
		case 'ж':
			b.WriteString("ž")
		case 'з':
			b.WriteString("z")
		case 'и':
			b.WriteString("i")
		case 'ј':
			b.WriteString("j")
		case 'к':
			b.WriteString("k")
		case 'л':
			b.WriteString("l")
		case 'љ':
			b.WriteString("lj")
		case 'м':
			b.WriteString("m")
		case 'н':
			b.WriteString("n")
		case 'њ':
			b.WriteString("nj")
		case 'о':
			b.WriteString("o")
		case 'п':
			b.WriteString("p")
		case 'р':
			b.WriteString("r")
		case 'с':
			b.WriteString("s")
		case 'т':
			b.WriteString("t")
		case 'ћ':
			b.WriteString("ć")
		case 'у':
			b.WriteString("u")
		case 'ф':
			b.WriteString("f")
		case 'х':
			b.WriteString("h")
		case 'ц':
			b.WriteString("c")
		case 'ч':
			b.WriteString("č")
		case 'џ':
			b.WriteString("dž")
		case 'ш':
			b.WriteString("š")
		// uppercase
		case 'А':
			b.WriteString("A")
		case 'Б':
			b.WriteString("B")
		case 'В':
			b.WriteString("V")
		case 'Г':
			b.WriteString("G")
		case 'Д':
			b.WriteString("D")
		case 'Ђ':
			b.WriteString("Đ")
		case 'Е':
			b.WriteString("E")
		case 'Ж':
			b.WriteString("Ž")
		case 'З':
			b.WriteString("Z")
		case 'И':
			b.WriteString("I")
		case 'Ј':
			b.WriteString("J")
		case 'К':
			b.WriteString("K")
		case 'Л':
			b.WriteString("L")
		case 'Љ':
			b.WriteString("Lj")
		case 'М':
			b.WriteString("M")
		case 'Н':
			b.WriteString("N")
		case 'Њ':
			b.WriteString("Nj")
		case 'О':
			b.WriteString("O")
		case 'П':
			b.WriteString("P")
		case 'Р':
			b.WriteString("R")
		case 'С':
			b.WriteString("S")
		case 'Т':
			b.WriteString("T")
		case 'Ћ':
			b.WriteString("Ć")
		case 'У':
			b.WriteString("U")
		case 'Ф':
			b.WriteString("F")
		case 'Х':
			b.WriteString("H")
		case 'Ц':
			b.WriteString("C")
		case 'Ч':
			b.WriteString("Č")
		case 'Џ':
			b.WriteString("Dž")
		case 'Ш':
			b.WriteString("Š")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toCyrillicSerbian transliterates Serbian Latin (with diacritics) to Serbian Cyrillic.
// It handles digraphs lj/nj/dž with casing.
func toCyrillicSerbian(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		// Try trigraph/digraph first: Dž/dž/DŽ, Nj/nj/NJ, Lj/lj/LJ
		if i+2 <= len(s) {
			two := s[i : i+2]
			switch two {
			case "nj":
				out.WriteRune('њ')
				i += 2
				continue
			case "Nj":
				out.WriteRune('Њ')
				i += 2
				continue
			case "NJ":
				out.WriteRune('Њ')
				i += 2
				continue
			case "lj":
				out.WriteRune('љ')
				i += 2
				continue
			case "Lj":
				out.WriteRune('Љ')
				i += 2
				continue
			case "LJ":
				out.WriteRune('Љ')
				i += 2
				continue
			case "dž":
				out.WriteRune('џ')
				i += 2
				continue
			case "Dž":
				out.WriteRune('Џ')
				i += 2
				continue
			case "DŽ":
				out.WriteRune('Џ')
				i += 2
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		// lowercase
		case 'a':
			out.WriteRune('а')
		case 'b':
			out.WriteRune('б')
		case 'v':
			out.WriteRune('в')
		case 'g':
			out.WriteRune('г')
		case 'd':
			out.WriteRune('д')
		case 'đ':
			out.WriteRune('ђ')
		case 'e':
			out.WriteRune('е')
		case 'ž':
			out.WriteRune('ж')
		case 'z':
			out.WriteRune('з')
		case 'i':
			out.WriteRune('и')
		case 'j':
			out.WriteRune('ј')
		case 'k':
			out.WriteRune('к')
		case 'l':
			out.WriteRune('л')
		case 'm':
			out.WriteRune('м')
		case 'n':
			out.WriteRune('н')
		case 'o':
			out.WriteRune('о')
		case 'p':
			out.WriteRune('п')
		case 'r':
			out.WriteRune('р')
		case 's':
			out.WriteRune('с')
		case 't':
			out.WriteRune('т')
		case 'ć':
			out.WriteRune('ћ')
		case 'u':
			out.WriteRune('у')
		case 'f':
			out.WriteRune('ф')
		case 'h':
			out.WriteRune('х')
		case 'c':
			out.WriteRune('ц')
		case 'č':
			out.WriteRune('ч')
		case 'š':
			out.WriteRune('ш')
		// uppercase
		case 'A':
			out.WriteRune('А')
		case 'B':
			out.WriteRune('Б')
		case 'V':
			out.WriteRune('В')
		case 'G':
			out.WriteRune('Г')
		case 'D':
			out.WriteRune('Д')
		case 'Đ':
			out.WriteRune('Ђ')
		case 'E':
			out.WriteRune('Е')
		case 'Ž':
			out.WriteRune('Ж')
		case 'Z':
			out.WriteRune('З')
		case 'I':
			out.WriteRune('И')
		case 'J':
			out.WriteRune('Ј')
		case 'K':
			out.WriteRune('К')
		case 'L':
			out.WriteRune('Л')
		case 'M':
			out.WriteRune('М')
		case 'N':
			out.WriteRune('Н')
		case 'O':
			out.WriteRune('О')
		case 'P':
			out.WriteRune('П')
		case 'R':
			out.WriteRune('Р')
		case 'S':
			out.WriteRune('С')
		case 'T':
			out.WriteRune('Т')
		case 'Ć':
			out.WriteRune('Ћ')
		case 'U':
			out.WriteRune('У')
		case 'F':
			out.WriteRune('Ф')
		case 'H':
			out.WriteRune('Х')
		case 'C':
			out.WriteRune('Ц')
		case 'Č':
			out.WriteRune('Ч')
		case 'Š':
			out.WriteRune('Ш')
		default:
			out.WriteRune(r)
		}
		i += size
	}
	return out.String()
}
