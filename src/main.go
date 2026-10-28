package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Listen        string `json:"listen"`
	APIKey        string `json:"api_key"`
	AdminKey      string `json:"admin_key"`
	DefaultModel  string `json:"default_model"`
	TimeoutSecond int    `json:"timeout_seconds"`
	AutoCapture   bool   `json:"auto_capture"`
	Upstream      struct {
		Base      string `json:"base"`
		Cookie    string `json:"cookie"`
		UserID    string `json:"user_id"`
		UserName  string `json:"user_name"`
		ProjectID string `json:"project_id"`
		UserAgent string `json:"user_agent"`
		Source    string `json:"source"`
	} `json:"upstream"`
	CookieSources []string `json:"cookie_sources"`
}

type Model struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	ModelID string `json:"modelId"`
}

type messageContent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt"`
}

// Timing 记录一次请求的耗时（毫秒），用于测速。
type Timing struct {
	QueueMS int64
	GenMS   int64
	WallMS  int64
}

type upstreamMessage struct {
	Role      string           `json:"role"`
	Content   []messageContent `json:"content"`
	Timestamp int64            `json:"timestamp"`
}

type messagesResp struct {
	Code int `json:"code"`
	Data struct {
		Messages   []upstreamMessage `json:"messages"`
		Model      string            `json:"model"`
		StopReason string            `json:"stopReason"`
	} `json:"data"`
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Event struct {
	At    string `json:"at"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type Server struct {
	cfgPath string
	cfg     *Config
	cfgMu   sync.RWMutex

	client *http.Client

	catMu  sync.Mutex
	models []Model
	catAt  time.Time

	slots  int
	inflMu sync.Mutex
	infl   []*inflight
	inflSeq int64

	statMu    sync.Mutex
	okCount   int
	failCount int
	lastErr   string
	lastAt    time.Time

	evMu sync.Mutex
	evs  []Event
}

// inflight 是一个正在执行的对话任务，可被更新的请求抢占。
type inflight struct {
	id      int64
	started time.Time
	ctx     context.Context
	cancel  context.CancelFunc
}

// errPreempted 表示该请求因并发槽被占而被更新的请求顶掉。
var errPreempted = errors.New("该请求已被更新的请求抢占")

// acquire 申请一个并发槽：槽满时抢占最旧的任务（取消它），再放入新任务。
func (s *Server) acquire() (job *inflight, evicted *inflight) {
	s.inflMu.Lock()
	defer s.inflMu.Unlock()
	if s.slots > 0 && len(s.infl) >= s.slots {
		evicted = s.infl[0]
		s.infl = s.infl[1:]
		evicted.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	job = &inflight{id: atomic.AddInt64(&s.inflSeq, 1), started: time.Now(), ctx: ctx, cancel: cancel}
	s.infl = append(s.infl, job)
	return job, evicted
}

func (s *Server) release(job *inflight) {
	job.cancel()
	s.inflMu.Lock()
	defer s.inflMu.Unlock()
	for i, f := range s.infl {
		if f == job {
			s.infl = append(s.infl[:i], s.infl[i+1:]...)
			return
		}
	}
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	applyDefaults(cfg)
	if cfg.Upstream.ProjectID == "" {
		log.Fatal("配置缺少 upstream.project_id：请填对话页 URL 里 pid 的值")
	}
	if cfg.Upstream.Cookie == "" && !cfg.AutoCapture {
		log.Println("警告: 既没有 upstream.cookie 也没有开启 auto_capture，请求会失败")
	}

	s := &Server{
		cfgPath: *cfgPath,
		cfg:     cfg,
		client: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConns:    16,
			},
		},
		slots: 4,
	}
	if cfg.AdminKey == "" {
		cfg.AdminKey = randHex(16)
		s.saveConfigLocked()
	}

	s.event("info", "服务启动，监听 "+cfg.Listen)
	if cfg.AutoCapture {
		go func() {
			if msg, _, err := s.capture(); err != nil {
				s.event("warn", "自动抓取凭证失败: "+err.Error())
			} else {
				s.event("ok", "自动抓取凭证: "+msg)
			}
			for range time.Tick(10 * time.Minute) {
				if _, info, err := s.capture(); err == nil && info != nil {
					s.event("ok", "凭证已同步，有效期至 "+time.Unix(info.Exp, 0).Format("2006-01-02 15:04"))
				}
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", s.auth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.auth(s.handleChat))
	mux.HandleFunc("/admin", s.handleAdminPage)
	mux.HandleFunc("/admin/", s.handleAdminPage)
	mux.HandleFunc("/admin/api/status", s.adminAuth(s.apiStatus))
	mux.HandleFunc("/admin/api/capture", s.adminAuth(s.apiCapture))
	mux.HandleFunc("/admin/api/open-login", s.adminAuth(s.apiOpenLogin))
	mux.HandleFunc("/admin/api/test", s.adminAuth(s.apiTest))
	mux.HandleFunc("/admin/api/speed", s.adminAuth(s.apiSpeed))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	log.Printf("监听 %s，默认模型 %s，上游 %s", cfg.Listen, cfg.DefaultModel, cfg.Upstream.Base)
	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func applyDefaults(c *Config) {
	if c.TimeoutSecond < 0 {
		c.TimeoutSecond = 900
	}
	if c.Upstream.Base == "" {
		c.Upstream.Base = "https://aistudio.ai4s.com.cn/partner/pi-agent/api"
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "auto"
	}
	if len(c.CookieSources) == 0 {
		home, _ := os.UserHomeDir()
		c.CookieSources = []string{
			filepath.Join(home, "Library/Application Support/Trae CN/Partitions/trae-webview/Cookies"),
			filepath.Join(home, "Library/Application Support/Trae CN/Cookies"),
		}
	}
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Server) snapshot() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return *s.cfg
}

func (s *Server) updateConfig(fn func(c *Config)) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	fn(s.cfg)
	return s.saveConfigLocked()
}

func (s *Server) saveConfigLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	s.catMu.Lock()
	s.models, s.catAt = nil, time.Time{}
	s.catMu.Unlock()
	return os.Rename(tmp, s.cfgPath)
}

func (s *Server) event(level, msg string) {
	log.Printf("[%s] %s", level, msg)
	s.evMu.Lock()
	defer s.evMu.Unlock()
	s.evs = append(s.evs, Event{At: time.Now().Format("15:04:05"), Level: level, Msg: msg})
	if len(s.evs) > 200 {
		s.evs = s.evs[len(s.evs)-200:]
	}
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" {
			tok = r.Header.Get("X-Api-Key")
		}
		if tok != s.snapshot().APIKey {
			writeErr(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		next(w, r)
	}
}

func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Admin-Key")
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if key != s.snapshot().AdminKey {
			writeErr(w, http.StatusUnauthorized, "invalid admin key")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, nil)
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshot()
	info := parseJWT(cfg.Upstream.Cookie)
	ms, err := s.catalog()
	models := make([]map[string]string, 0, len(ms))
	for _, m := range ms {
		models = append(models, map[string]string{"id": m.ModelID, "label": m.Label})
	}
	s.statMu.Lock()
	stats := map[string]any{
		"ok": s.okCount, "fail": s.failCount, "last_error": s.lastErr,
		"last_at": s.lastAt.Format("2006-01-02 15:04:05"),
	}
	s.statMu.Unlock()
	s.evMu.Lock()
	evs := append([]Event(nil), s.evs...)
	s.evMu.Unlock()

	account := map[string]any{"logged_in": false}
	if info != nil {
		account = map[string]any{
			"logged_in": info.Exp > time.Now().Unix(),
			"name":      info.Name,
			"uid":       info.UID,
			"role":      info.Role,
			"exp":       time.Unix(info.Exp, 0).Format("2006-01-02 15:04"),
			"exp_ts":    info.Exp,
			"source":    cfg.Upstream.Source,
		}
	}
	resp := map[string]any{
		"account":     account,
		"models":      models,
		"stats":       stats,
		"events":      evs,
		"base_url":    "http://" + cfg.Listen + "/v1",
		"api_key":     cfg.APIKey,
		"default":     cfg.DefaultModel,
		"models_err":  "",
	}
	if err != nil {
		resp["models_err"] = err.Error()
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) apiCapture(w http.ResponseWriter, r *http.Request) {
	msg, info, err := s.capture()
	if err != nil {
		s.event("error", "抓取凭证失败: "+err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.event("ok", "抓取凭证成功: "+msg)
	json.NewEncoder(w).Encode(map[string]any{
		"message": msg,
		"name":    info.Name,
		"uid":     info.UID,
		"exp":     time.Unix(info.Exp, 0).Format("2006-01-02 15:04"),
	})
}

func (s *Server) apiOpenLogin(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("open", "https://aistudio.ai4s.com.cn/login")
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, "打开浏览器失败: "+err.Error())
		return
	}
	s.event("info", "已打开星河启智登录页")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) apiTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	model := body.Model
	if model == "" {
		model = s.snapshot().DefaultModel
	}
	text, modelID, err := s.complete(model, []chatMessage{{Role: "user", Content: "只回答四个字：连接正常"}})
	if err != nil {
		s.recordResult(false, err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.recordResult(true, "")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": modelID, "reply": text})
}

// apiSpeed 让指定模型生成一段固定篇幅的中文，用上游返回的 startedAt/endedAt
// 计算生成窗口，按估算 token 数换算出粗略 TPS。
func (s *Server) apiSpeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = s.snapshot().DefaultModel
	}
	const speedPrompt = "请用中文写一段约 200 字的科普短文，介绍潮汐是怎么形成的。只输出正文，不要标题、不要列表。"
	text, modelID, t, err := s.completeTimed(model, []chatMessage{{Role: "user", Content: speedPrompt}})
	if err != nil {
		s.recordResult(false, err.Error())
		s.event("error", "测速失败: "+err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.recordResult(true, "")
	chars := len([]rune(text))
	tokens := estimateTokens(text)
	genSec := float64(t.GenMS) / 1000
	if genSec <= 0 {
		genSec = float64(t.WallMS) / 1000
	}
	tps := 0.0
	if genSec > 0 {
		tps = float64(tokens) / genSec
	}
	s.event("info", fmt.Sprintf("测速 %s: %.1f tps（%d 字 / %.1f 秒）", modelID, tps, chars, genSec))
	json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"model":        modelID,
		"chars":        chars,
		"tokens":       tokens,
		"gen_seconds":  math.Round(genSec*100) / 100,
		"wall_seconds": math.Round(float64(t.WallMS)/10) / 100,
		"queue_ms":     t.QueueMS,
		"tps":          math.Round(tps*10) / 10,
		"preview":      truncate(text, 120),
	})
}

// estimateTokens 粗略估算 token 数：中日韩字符按 0.7 token/字，其余按 4 字符/token。
func estimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		switch {
		case r >= 0x2E80:
			cjk++
		case r == ' ' || r == '\n' || r == '\r' || r == '\t':
		default:
			other++
		}
	}
	t := float64(cjk)*0.7 + float64(other)/4
	if t < 1 {
		return 1
	}
	return int(math.Round(t))
}

// capture 从本机浏览器 Cookie 库中提取星河启智 JWT，并写入配置。
func (s *Server) capture() (string, *JWTInfo, error) {
	cfg := s.snapshot()
	var lastErr error
	for _, src := range cfg.CookieSources {
		if _, err := os.Stat(src); err != nil {
			continue
		}
		tok, err := readCookieDB(src)
		if err != nil {
			lastErr = err
			continue
		}
		if tok == "" {
			lastErr = errors.New("该 Cookie 库里没有星河启智登录态")
			continue
		}
		info := parseJWT("Authorization=" + tok)
		if info == nil {
			lastErr = errors.New("取到的不是有效 JWT")
			continue
		}
		if info.Exp <= time.Now().Unix() {
			lastErr = errors.New("登录态已过期，请重新登录")
			continue
		}
		// 校验上游是否接受
		if _, err := s.callWith("POST", "/models/search", map[string]any{"projectId": cfg.Upstream.ProjectID}, "Authorization="+tok); err != nil {
			lastErr = fmt.Errorf("上游校验失败: %w", err)
			continue
		}
		if err := s.updateConfig(func(c *Config) {
			c.Upstream.Cookie = "Authorization=" + tok
			c.Upstream.UserID = info.UID
			c.Upstream.UserName = info.Name
			c.Upstream.Source = src
		}); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("%s（%s）", src, info.Name), info, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有找到可用的浏览器 Cookie 库")
	}
	return "", nil, lastErr
}

func readCookieDB(path string) (string, error) {
	dir, err := os.MkdirTemp("", "cookie")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	dst := filepath.Join(dir, "Cookies")
	if err := copyFile(path, dst); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := "SELECT value FROM cookies WHERE name='Authorization' AND host_key LIKE '%ai4s%' ORDER BY expires_utc DESC LIMIT 1;"
	out, err := exec.CommandContext(ctx, "sqlite3", "-json", dst, q).Output()
	if err != nil {
		return "", fmt.Errorf("读取 Cookie 库失败: %v", err)
	}
	var rows []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil || len(rows) == 0 {
		return "", nil
	}
	return strings.TrimSpace(rows[0].Value), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

type JWTInfo struct {
	UID   string `json:"X-User-Id"`
	Name  string `json:"X-User-Name"`
	Role  string `json:"X-User-Role"`
	Iss   string `json:"iss"`
	Exp   int64  `json:"exp"`
	Iat   int64  `json:"iat"`
	Jti   string `json:"jti"`
}

func parseJWT(cookie string) *JWTInfo {
	v := strings.TrimSpace(strings.TrimPrefix(cookie, "Authorization="))
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var j JWTInfo
	if json.Unmarshal(raw, &j) != nil || j.Exp == 0 {
		return nil
	}
	return &j
}

func (s *Server) recordResult(ok bool, errMsg string) {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	if ok {
		s.okCount++
	} else {
		s.failCount++
		s.lastErr = errMsg
	}
	s.lastAt = time.Now()
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.catalog()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := []item{}
	for _, m := range ms {
		out = append(out, item{ID: m.ModelID, Object: "model", OwnedBy: "sais"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	msgs := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: m.Role, Content: contentText(m.Content)})
	}
	if len(msgs) == 0 {
		writeErr(w, http.StatusBadRequest, "messages is empty")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" || model == "auto" {
		model = s.snapshot().DefaultModel
	}

	job, evicted := s.acquire()
	defer s.release(job)
	if evicted != nil {
		s.event("warn", fmt.Sprintf("并发槽已满，抢占最旧请求 #%d（已等待 %s）", evicted.id,
			time.Since(evicted.started).Round(time.Second)))
	}

	text, modelID, err := s.completeCtx(job.ctx, model, msgs)
	if err != nil {
		if errors.Is(err, errPreempted) {
			s.event("warn", fmt.Sprintf("请求 #%d 被新请求抢占，已中止", job.id))
			writeErr(w, http.StatusServiceUnavailable, errPreempted.Error())
			return
		}
		s.recordResult(false, err.Error())
		s.event("error", "对话失败: "+err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.recordResult(true, "")

	if req.Stream {
		streamResult(w, modelID, text)
		return
	}

	type choice struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}
	var resp struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
		Usage   struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	resp.ID = "chatcmpl-" + randHex(8)
	resp.Object = "chat.completion"
	resp.Created = time.Now().Unix()
	resp.Model = modelID
	resp.Choices = []choice{{Index: 0, Message: chatMessage{Role: "assistant", Content: text}, FinishReason: "stop"}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) complete(model string, msgs []chatMessage) (string, string, error) {
	return s.completeCtx(context.Background(), model, msgs)
}

func (s *Server) completeCtx(ctx context.Context, model string, msgs []chatMessage) (string, string, error) {
	text, modelID, _, err := s.completeTimedCtx(ctx, model, msgs)
	return text, modelID, err
}

func (s *Server) completeTimed(model string, msgs []chatMessage) (string, string, Timing, error) {
	return s.completeTimedCtx(context.Background(), model, msgs)
}

func (s *Server) completeTimedCtx(ctx context.Context, model string, msgs []chatMessage) (string, string, Timing, error) {
	cfg := s.snapshot()
	modelID, modelConfigID, err := s.resolveModel(model)
	if err != nil {
		return "", "", Timing{}, err
	}
	pid := cfg.Upstream.ProjectID
	sid := newSessionID()

	if _, err := s.callCtx(ctx, "POST", "/sessions", map[string]any{
		"projectId": pid, "session_id": sid, "model": modelID,
	}); err != nil {
		return "", "", Timing{}, fmt.Errorf("创建会话失败: %w", err)
	}
	if _, err := s.callCtx(ctx, "POST", "/sessions/prompt", map[string]any{
		"message":       buildPrompt(msgs),
		"modelConfigId": modelConfigID,
		"projectId":     pid,
		"session_id":    sid,
	}); err != nil {
		return "", "", Timing{}, fmt.Errorf("发送消息失败: %w", err)
	}
	promptAt := time.Now()

	deadline := time.Time{}
	if cfg.TimeoutSecond > 0 {
		deadline = promptAt.Add(time.Duration(cfg.TimeoutSecond) * time.Second)
	}
	var lastErr string
	for deadline.IsZero() || time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", "", Timing{}, errPreempted
		case <-time.After(900 * time.Millisecond):
		}
		body, err := s.callCtx(ctx, "GET", "/sessions/messages?projectId="+pid+"&session_id="+sid, nil)
		if err != nil {
			if ctx.Err() != nil {
				return "", "", Timing{}, errPreempted
			}
			if err.Error() != lastErr {
				log.Printf("轮询失败(%s): %v", sid, err)
				lastErr = err.Error()
			}
			continue
		}
		var mr messagesResp
		if json.Unmarshal(body, &mr) != nil {
			continue
		}
		if len(mr.Data.Messages) == 0 {
			continue
		}
		last := mr.Data.Messages[len(mr.Data.Messages)-1]
		if last.Role != "assistant" {
			continue
		}
		text := extractText(last)
		if text == "" {
			continue
		}
		if mr.Data.StopReason != "" || lastEnded(last) {
			t := Timing{WallMS: time.Since(promptAt).Milliseconds()}
			var minStart, maxEnd int64
			for _, c := range last.Content {
				if c.StartedAt > 0 && (minStart == 0 || c.StartedAt < minStart) {
					minStart = c.StartedAt
				}
				if c.EndedAt > maxEnd {
					maxEnd = c.EndedAt
				}
			}
			if minStart > 0 && maxEnd > minStart {
				t.GenMS = maxEnd - minStart
				if ms := promptAt.UnixMilli(); minStart > ms {
					t.QueueMS = minStart - ms
				}
			} else {
				t.GenMS = t.WallMS
			}
			return text, modelID, t, nil
		}
	}
	return "", "", Timing{}, errors.New("等待模型回复超时")
}

func (s *Server) resolveModel(name string) (modelID, configID string, err error) {
	ms, err := s.catalog()
	if err != nil {
		return "", "", err
	}
	if name == "" || name == "auto" {
		return ms[0].ModelID, ms[0].ID, nil
	}
	for _, m := range ms {
		if m.ModelID == name || m.Label == name {
			return m.ModelID, m.ID, nil
		}
	}
	return "", "", fmt.Errorf("未知模型 %q，可用: %s", name, modelNames(ms))
}

func (s *Server) catalog() ([]Model, error) {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if len(s.models) > 0 && time.Since(s.catAt) < 10*time.Minute {
		return s.models, nil
	}
	body, err := s.call("POST", "/models/search", map[string]any{"projectId": s.snapshot().Upstream.ProjectID})
	if err != nil {
		if len(s.models) > 0 {
			return s.models, nil
		}
		return nil, fmt.Errorf("获取模型列表失败: %w", err)
	}
	var r struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.Data) == 0 {
		return nil, errors.New("模型列表为空")
	}
	s.models, s.catAt = r.Data, time.Now()
	return s.models, nil
}

func (s *Server) call(method, path string, payload any) ([]byte, error) {
	return s.callWith(method, path, payload, s.snapshot().Upstream.Cookie)
}

func (s *Server) callWith(method, path string, payload any, cookie string) ([]byte, error) {
	return s.callWithCtx(context.Background(), method, path, payload, cookie)
}

func (s *Server) callCtx(ctx context.Context, method, path string, payload any) ([]byte, error) {
	return s.callWithCtx(ctx, method, path, payload, s.snapshot().Upstream.Cookie)
}

func (s *Server) callWithCtx(ctx context.Context, method, path string, payload any, cookie string) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	cfg := s.snapshot()
	req, err := http.NewRequestWithContext(ctx, method, cfg.Upstream.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("x-user-id", cfg.Upstream.UserID)
	req.Header.Set("x-user-name", cfg.Upstream.UserName)
	req.Header.Set("x-user-role", "USER_ROLE_MEMBER")
	req.Header.Set("x-auth-status", "1")
	req.Header.Set("x-user-type", "client")
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ua := cfg.Upstream.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return b, nil
}

func buildPrompt(msgs []chatMessage) string {
	var sys []string
	last := msgs[len(msgs)-1]
	for _, m := range msgs[:len(msgs)-1] {
		if m.Role == "system" {
			sys = append(sys, m.Content)
		}
	}
	var b strings.Builder
	for _, x := range sys {
		b.WriteString(x)
		b.WriteString("\n\n")
	}
	if len(msgs) > 1 {
		b.WriteString("[对话历史]\n")
		for _, m := range msgs[:len(msgs)-1] {
			switch m.Role {
			case "user":
				b.WriteString("用户: ")
			case "assistant":
				b.WriteString("助手: ")
			case "system":
				continue
			default:
				b.WriteString(m.Role + ": ")
			}
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
		b.WriteString("\n[当前问题]\n")
	}
	b.WriteString(last.Content)
	return b.String()
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func extractText(m upstreamMessage) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func lastEnded(m upstreamMessage) bool {
	for _, c := range m.Content {
		if c.Type == "text" && c.EndedAt > 0 {
			return true
		}
	}
	return false
}

func streamResult(w http.ResponseWriter, model, text string) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	id := "chatcmpl-" + randHex(8)
	send := func(delta map[string]any, finish any) {
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher()
	}
	send(map[string]any{"role": "assistant", "content": ""}, nil)
	runes := []rune(text)
	for i := 0; i < len(runes); i += 24 {
		end := i + 24
		if end > len(runes) {
			end = len(runes)
		}
		send(map[string]any{"content": string(runes[i:end])}, nil)
		time.Sleep(15 * time.Millisecond)
	}
	send(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher()
}

// newSessionID 生成 18 位纯数字会话 ID（上游要求路径安全片段）。
func newSessionID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(900000000000000000))
	return fmt.Sprintf("%d", 100000000000000000+n.Int64())
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func modelNames(ms []Model) string {
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.ModelID)
	}
	return strings.Join(names, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": "upstream_error"}})
	w.Write(b)
}

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>星河启智 反代控制台</title>
<style>
  :root{
    --bg:#0b0e14; --card:#141922; --card2:#1b2230; --line:#252d3d;
    --txt:#e6ebf5; --dim:#8b97ad; --accent:#5b8cff; --ok:#2fbf71; --warn:#f0a725; --err:#ef4b5f;
  }
  *{box-sizing:border-box}
  body{margin:0;background:radial-gradient(1200px 600px at 20% -10%,#1a2340 0%,var(--bg) 55%);
       color:var(--txt);font:14px/1.6 -apple-system,BlinkMacSystemFont,"PingFang SC","Helvetica Neue",sans-serif;
       min-height:100vh;padding:28px 18px 60px}
  .wrap{max-width:1080px;margin:0 auto}
  header{display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:22px}
  h1{font-size:20px;margin:0;letter-spacing:.5px}
  .badge{display:inline-flex;align-items:center;gap:6px;padding:4px 11px;border-radius:999px;
         font-size:12px;border:1px solid var(--line);background:var(--card)}
  .dot{width:7px;height:7px;border-radius:50%;background:var(--dim)}
  .dot.ok{background:var(--ok);box-shadow:0 0 8px var(--ok)}
  .dot.err{background:var(--err);box-shadow:0 0 8px var(--err)}
  .dot.warn{background:var(--warn);box-shadow:0 0 8px var(--warn)}
  .grid{display:grid;grid-template-columns:1fr 1fr;gap:16px}
  @media(max-width:820px){.grid{grid-template-columns:1fr}}
  .card{background:linear-gradient(180deg,var(--card) 0%,var(--card2) 100%);
        border:1px solid var(--line);border-radius:14px;padding:18px 20px}
  .card h2{font-size:13px;margin:0 0 14px;color:var(--dim);font-weight:600;letter-spacing:1px}
  .rows{display:flex;flex-direction:column;gap:9px}
  .row{display:flex;justify-content:space-between;gap:14px;font-size:13px}
  .row .k{color:var(--dim);flex:0 0 auto}
  .row .v{text-align:right;word-break:break-all;font-variant-numeric:tabular-nums}
  .mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
  .btns{display:flex;gap:10px;flex-wrap:wrap;margin-top:16px}
  button{cursor:pointer;border:1px solid var(--line);background:#222b3b;color:var(--txt);
         padding:8px 14px;border-radius:9px;font-size:13px;transition:.15s}
  button:hover{background:#2a354a;border-color:#3a4761}
  button.primary{background:linear-gradient(135deg,#4a7cff,#6a5cff);border-color:transparent;font-weight:600}
  button.primary:hover{filter:brightness(1.1)}
  button:disabled{opacity:.5;cursor:not-allowed}
  .chips{display:flex;flex-wrap:wrap;gap:8px}
  .chip{padding:5px 11px;border-radius:8px;background:#1e2636;border:1px solid var(--line);
        font-size:12px;font-family:ui-monospace,Menlo,monospace;color:#bfcbdf}
  .chip.default{border-color:var(--accent);color:#9dbaff}
  .speedbar{display:flex;align-items:center;gap:12px;flex-wrap:wrap}
  select{background:#0f141d;color:var(--txt);border:1px solid var(--line);border-radius:9px;
         padding:8px 12px;font-size:13px;font-family:ui-monospace,Menlo,monospace;outline:none}
  select:focus{border-color:var(--accent)}
  .speedout{margin-top:16px;display:none}
  .speedout.on{display:block}
  .tps{font-size:12px;color:var(--dim);display:flex;align-items:baseline;gap:8px}
  .tps b{font-size:32px;color:#9dbaff;letter-spacing:-1px;font-variant-numeric:tabular-nums}
  .smeta{margin-top:6px;font-size:12px;color:var(--dim)}
  .pv{margin-top:10px;font-size:12px;color:#a8b6cd;line-height:1.55;
      border-left:2px solid var(--line);padding-left:10px;white-space:pre-wrap}
  .kv{display:flex;align-items:center;gap:10px;background:#0f141d;border:1px solid var(--line);
      border-radius:9px;padding:9px 12px;margin-bottom:10px}
  .kv span{flex:1;word-break:break-all;font-family:ui-monospace,Menlo,monospace;font-size:12px}
  .kv button{padding:4px 10px;font-size:12px}
  .events{max-height:260px;overflow:auto;display:flex;flex-direction:column;gap:6px}
  .ev{display:flex;gap:10px;font-size:12px;padding:5px 8px;border-radius:6px;background:#10151e}
  .ev .t{color:var(--dim);font-family:ui-monospace,Menlo,monospace;flex:0 0 auto}
  .ev.ok .m{color:#7fe0a8}
  .ev.warn .m{color:#f5c26b}
  .ev.error .m{color:#ff8092}
  .ev.info .m{color:#a8b6cd}
  .tip{color:var(--dim);font-size:12px;margin-top:10px}
  ol{margin:0;padding-left:20px;color:var(--dim);font-size:13px}
  ol b{color:var(--txt)}
  .toast{position:fixed;left:50%;bottom:28px;transform:translateX(-50%) translateY(20px);
         background:#243049;border:1px solid #3a4761;color:var(--txt);padding:10px 18px;border-radius:10px;
         font-size:13px;opacity:0;transition:.25s;pointer-events:none;z-index:99}
  .toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
  .full{grid-column:1/-1}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>星河启智 · 反代控制台</h1>
    <span class="badge"><span class="dot" id="dot"></span><span id="stateText">连接中…</span></span>
  </header>

  <div class="grid">
    <div class="card">
      <h2>账户信息</h2>
      <div class="rows">
        <div class="row"><span class="k">登录状态</span><span class="v" id="loginState">—</span></div>
        <div class="row"><span class="k">用户名</span><span class="v" id="userName">—</span></div>
        <div class="row"><span class="k">用户 ID</span><span class="v mono" id="userId">—</span></div>
        <div class="row"><span class="k">角色</span><span class="v" id="userRole">—</span></div>
        <div class="row"><span class="k">凭证来源</span><span class="v mono" id="userSource">—</span></div>
        <div class="row"><span class="k">有效期至</span><span class="v" id="userExp">—</span></div>
      </div>
      <div class="btns">
        <button class="primary" id="btnLogin">打开登录页</button>
        <button id="btnCapture">抓取凭证</button>
        <button id="btnTest">测试对话</button>
      </div>
      <div class="tip">凭证来自本机浏览器的登录 Cookie：在浏览器里登录一次星河启智，点「抓取凭证」即可自动同步。</div>
    </div>

    <div class="card">
      <h2>接入信息（OpenAI 兼容）</h2>
      <div class="kv"><span id="baseUrl">—</span><button data-copy="baseUrl">复制</button></div>
      <div class="kv"><span id="apiKey">—</span><button data-copy="apiKey">复制</button></div>
      <div class="rows">
        <div class="row"><span class="k">默认模型</span><span class="v mono" id="defaultModel">—</span></div>
        <div class="row"><span class="k">请求统计</span><span class="v" id="stats">—</span></div>
        <div class="row"><span class="k">最近错误</span><span class="v" id="lastErr">—</span></div>
      </div>
      <div class="tip">把 Base URL 与 API Key 填进任意 OpenAI 客户端即可使用。</div>
    </div>

    <div class="card">
      <h2>可用模型</h2>
      <div class="chips" id="models"><span class="chip">加载中…</span></div>
      <div class="tip" id="modelsErr"></div>
    </div>

    <div class="card">
      <h2>最近事件</h2>
      <div class="events" id="events"></div>
    </div>

    <div class="card full">
      <h2>速率测试（粗略 TPS）</h2>
      <div class="speedbar">
        <select id="speedModel"></select>
        <button class="primary" id="btnSpeed">开始测速</button>
        <span class="tip" style="margin:0">固定生成约 200 字中文，按上游 startedAt/endedAt 计算生成窗口，token 数为估算值。</span>
      </div>
      <div class="speedout" id="speedOut">
        <div class="tps"><b id="speedTps">—</b>tokens/s</div>
        <div class="smeta" id="speedMeta"></div>
        <div class="pv" id="speedPreview"></div>
      </div>
    </div>

    <div class="card full">
      <h2>三步开通</h2>
      <ol>
        <li>点 <b>打开登录页</b>，在浏览器里完成星河启智登录。</li>
        <li>回到本页点 <b>抓取凭证</b>，服务会从浏览器 Cookie 库读取登录态并自动校验。</li>
        <li>把上面的 <b>Base URL</b> 和 <b>API Key</b> 填进客户端即可反向调用账号额度。</li>
      </ol>
    </div>
  </div>
</div>
<div class="toast" id="toast"></div>

<script>
(function(){
  var storeKey = 'sais2api_admin_key';
  var qs = new URLSearchParams(location.search);
  if (qs.get('key')) { localStorage.setItem(storeKey, qs.get('key')); }
  var adminKey = localStorage.getItem(storeKey) || '';
  var $ = function(id){ return document.getElementById(id); };

  function toast(msg){
    var t = $('toast');
    t.textContent = msg; t.classList.add('show');
    clearTimeout(t._h);
    t._h = setTimeout(function(){ t.classList.remove('show'); }, 2200);
  }

  function api(path, body){
    return fetch(path, {
      method: 'POST',
      headers: {'X-Admin-Key': adminKey, 'Content-Type': 'application/json'},
      body: body ? JSON.stringify(body) : '{}'
    }).then(function(r){
      return r.json().catch(function(){ return {}; }).then(function(j){
        if (!r.ok) { throw new Error((j.error && j.error.message) || ('HTTP ' + r.status)); }
        return j;
      });
    });
  }

  function setState(kind, text){
    $('dot').className = 'dot ' + kind;
    $('stateText').textContent = text;
  }

  function fmtRest(ts){
    if (!ts) return '—';
    var diff = ts * 1000 - Date.now();
    if (diff <= 0) return '已过期';
    var d = Math.floor(diff / 86400000);
    var h = Math.floor((diff % 86400000) / 3600000);
    return d > 0 ? (d + ' 天 ' + h + ' 小时') : (h + ' 小时');
  }

  function render(s){
    var a = s.account || {};
    $('loginState').textContent = a.logged_in ? '已登录' : '未登录 / 已过期';
    $('loginState').style.color = a.logged_in ? 'var(--ok)' : 'var(--err)';
    $('userName').textContent = a.name || '—';
    $('userId').textContent = a.uid || '—';
    $('userRole').textContent = a.role || '—';
    $('userSource').textContent = a.source || '—';
    $('userExp').textContent = a.exp ? (a.exp + '（剩余 ' + fmtRest(a.exp_ts) + '）') : '—';
    $('baseUrl').textContent = s.base_url || '—';
    $('apiKey').textContent = s.api_key || '—';
    $('defaultModel').textContent = s.default || '—';

    var st = s.stats || {};
    $('stats').textContent = '成功 ' + (st.ok||0) + ' / 失败 ' + (st.fail||0) +
      (st.last_at ? '（' + st.last_at + '）' : '');
    $('lastErr').textContent = st.last_error || '无';

    var ms = s.models || [];
    var box = $('models'); box.innerHTML = '';
    if (!ms.length) {
      var c0 = document.createElement('span');
      c0.className = 'chip'; c0.textContent = '暂无模型';
      box.appendChild(c0);
    } else {
      ms.forEach(function(m){
        var c = document.createElement('span');
        c.className = 'chip' + (m.id === s.default ? ' default' : '');
        c.textContent = m.id + (m.label && m.label !== m.id ? ' · ' + m.label : '');
        box.appendChild(c);
      });
    }
    $('modelsErr').textContent = s.models_err ? ('模型列表获取失败：' + s.models_err) : '';

    var sel = $('speedModel');
    var want = sel.value || s.default;
    var key = ms.map(function(m){ return m.id; }).join(',');
    if (sel._key !== key) {
      sel._key = key;
      sel.innerHTML = '';
      ms.forEach(function(m){
        var o = document.createElement('option');
        o.value = m.id; o.textContent = m.id;
        sel.appendChild(o);
      });
      if (want) { sel.value = want; }
      if (!sel.value && s.default) { sel.value = s.default; }
    }

    var evs = (s.events || []).slice(-40).reverse();
    var eb = $('events'); eb.innerHTML = '';
    if (!evs.length) {
      eb.innerHTML = '<div class="ev info"><span class="m">暂无事件</span></div>';
    } else {
      evs.forEach(function(e){
        var d = document.createElement('div');
        d.className = 'ev ' + (e.level || 'info');
        d.innerHTML = '<span class="t"></span><span class="m"></span>';
        d.children[0].textContent = e.at;
        d.children[1].textContent = e.msg;
        eb.appendChild(d);
      });
    }

    if (a.logged_in) { setState('ok', '运行中 · 凭证有效'); }
    else { setState('warn', '运行中 · 需要登录'); }
  }

  function refresh(){
    if (!adminKey) { setState('err', '缺少管理密钥'); return; }
    api('/admin/api/status').then(render).catch(function(e){
      if (String(e.message).indexOf('admin key') >= 0) { setState('err', '管理密钥无效'); }
      else { setState('err', e.message); }
    });
  }

  $('btnLogin').onclick = function(){
    var b = this; b.disabled = true;
    api('/admin/api/open-login').then(function(){
      toast('已打开登录页，登录后再点「抓取凭证」');
    }).catch(function(e){ toast('失败：' + e.message); })
      .then(function(){ b.disabled = false; });
  };

  $('btnCapture').onclick = function(){
    var b = this; b.disabled = true; b.textContent = '抓取中…';
    api('/admin/api/capture').then(function(j){
      toast('已抓取：' + (j.name || '') + ' 有效期至 ' + (j.exp || ''));
      refresh();
    }).catch(function(e){ toast('抓取失败：' + e.message); })
      .then(function(){ b.disabled = false; b.textContent = '抓取凭证'; });
  };

  $('btnTest').onclick = function(){
    var b = this; b.disabled = true; b.textContent = '测试中…';
    api('/admin/api/test', {}).then(function(j){
      toast('模型回复：' + (j.reply || '').slice(0, 40));
      refresh();
    }).catch(function(e){ toast('测试失败：' + e.message); refresh(); })
      .then(function(){ b.disabled = false; b.textContent = '测试对话'; });
  };

  $('btnSpeed').onclick = function(){
    var b = this; b.disabled = true; b.textContent = '测速中…';
    var model = $('speedModel').value;
    $('speedOut').classList.add('on');
    $('speedTps').textContent = '…';
    $('speedMeta').textContent = '正在让 ' + model + ' 生成约 200 字，请稍候';
    $('speedPreview').textContent = '';
    api('/admin/api/speed', {model: model}).then(function(j){
      $('speedTps').textContent = Number(j.tps || 0).toFixed(1);
      $('speedMeta').textContent = j.model + ' · 输出约 ' + j.tokens + ' tokens（' + j.chars +
        ' 字） · 生成 ' + j.gen_seconds + ' 秒 · 总耗时 ' + j.wall_seconds + ' 秒' +
        (j.queue_ms ? '（其中排队 ' + (j.queue_ms / 1000).toFixed(1) + ' 秒）' : '');
      $('speedPreview').textContent = j.preview || '';
      refresh();
    }).catch(function(e){
      $('speedTps').textContent = '—';
      $('speedMeta').textContent = '测速失败：' + e.message;
    }).then(function(){ b.disabled = false; b.textContent = '开始测速'; });
  };

  document.querySelectorAll('[data-copy]').forEach(function(btn){
    btn.onclick = function(){
      var el = $(btn.getAttribute('data-copy'));
      var txt = el.textContent || '';
      if (navigator.clipboard) {
        navigator.clipboard.writeText(txt).then(function(){ toast('已复制'); });
      } else {
        var ta = document.createElement('textarea');
        ta.value = txt; document.body.appendChild(ta); ta.select();
        document.execCommand('copy'); document.body.removeChild(ta); toast('已复制');
      }
    };
  });

  refresh();
  setInterval(refresh, 5000);
})();
</script>
</body>
</html>`

var tmpl = template.Must(template.New("admin").Parse(adminHTML))
