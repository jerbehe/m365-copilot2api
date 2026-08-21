package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/auth"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type M365CloudClient struct {
	mu          sync.Mutex
	clientID    string
	tenantID    string
	accessToken string
	expiresAt   time.Time
	httpClient  *http.Client

	// creds 每次刷新前从账号存储读取当前 refresh token，refreshed 在换到新的
	// refresh token 后回写。微软的 refresh token 是滚动的：启动时快照一份自用，
	// 会与账号存储各自演进，其中一份迟早失效并让云端对话接口开始返回 401。
	creds     func() string
	refreshed func(string)
}

func NewM365CloudClient(clientID, tenantID, refreshToken string) *M365CloudClient {
	token := refreshToken
	return &M365CloudClient{
		clientID:   clientID,
		tenantID:   tenantID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		creds:      func() string { return token },
		refreshed:  func(s string) { token = s },
	}
}

// NewM365CloudClientWithStore 让 client 与账号存储共用同一份 refresh token。
func NewM365CloudClientWithStore(clientID, tenantID string, creds func() string, refreshed func(string)) *M365CloudClient {
	return &M365CloudClient{
		clientID:   clientID,
		tenantID:   tenantID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		creds:      creds,
		refreshed:  refreshed,
	}
}

func (c *M365CloudClient) getAccessToken() (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Now().Before(c.expiresAt.Add(-2*time.Minute)) {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt.Add(-2*time.Minute)) {
		return c.accessToken, nil
	}

	refreshToken := c.creds()
	if strings.TrimSpace(refreshToken) == "" {
		return "", fmt.Errorf("no refresh token available for account")
	}

	payload := url.Values{
		"client_id":     {c.clientID},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
		"scope":         {"https://m365.cloud.microsoft/v2/.default"},
	}.Encode()

	resp, err := c.httpClient.Post(
		fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", c.tenantID),
		"application/x-www-form-urlencoded",
		strings.NewReader(payload),
	)
	if err != nil {
		return "", fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if result.Error != "" {
		// 旧 token 已被上游拒绝，清掉缓存的 access token，避免继续拿它发请求。
		c.accessToken = ""
		return "", fmt.Errorf("token error: %s - %s", result.Error, result.ErrorDesc)
	}

	c.accessToken = result.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	if result.RefreshToken != "" && result.RefreshToken != refreshToken {
		c.refreshed(result.RefreshToken)
	}

	log.Printf("[m365-cloud] token refreshed, expires in %ds", result.ExpiresIn)
	return c.accessToken, nil
}

func (c *M365CloudClient) doAPI(action string, payload map[string]any) (map[string]any, error) {
	token, err := c.getAccessToken()
	if err != nil {
		return nil, err
	}

	reqBody := map[string]any{
		"action": action,
		"state":  payload,
	}
	for k, v := range payload {
		if k != "state" {
			reqBody[k] = v
		}
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://m365.cloud.microsoft/chat", strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		bodySnippet := ""
		if b, rerr := io.ReadAll(io.LimitReader(resp.Body, 512)); rerr == nil {
			bodySnippet = string(b)
		}
		return nil, &UpstreamHTTPError{
			Status:     resp.StatusCode,
			RetryAfter: retryAfter,
			Body:       bodySnippet,
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil, fmt.Errorf("unexpected content type from m365 endpoint: %s", resp.Header.Get("Content-Type"))
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse m365 response: %w", err)
	}

	return result, nil
}

func (c *M365CloudClient) DeleteConversation(conversationID string) error {
	log.Printf("[m365-cloud] deleting conversation %s", conversationID)
	_, err := c.doAPI("DeleteConversation", map[string]any{
		"conversationId": conversationID,
		"state": map[string]any{
			"conversationPageHistoryList": map[string]any{
				"chats": []any{},
			},
		},
	})
	return err
}

func (c *M365CloudClient) ListConversations() ([]map[string]any, error) {
	result, err := c.doAPI("RefreshNavPane", map[string]any{})
	if err != nil {
		return nil, err
	}

	store, ok := result["store"].(map[string]any)
	if !ok {
		log.Printf("[m365-cloud] unexpected response: %v", result)
		return nil, fmt.Errorf("unexpected response format")
	}

	historyRaw, present := store["conversationPageHistoryList"]
	if !present || historyRaw == nil {
		return []map[string]any{}, nil
	}
	historyList, ok := historyRaw.(map[string]any)
	if !ok {
		log.Printf("[m365-cloud] conversationPageHistoryList unexpected type: %T, treating as empty. store keys: %v", historyRaw, func() []string {
			keys := make([]string, 0)
			for k := range store {
				keys = append(keys, k)
			}
			return keys
		}())
		return []map[string]any{}, nil
	}

	chatsRaw, ok := historyList["chats"].([]any)
	if !ok {
		log.Printf("[m365-cloud] chats type: %T, value: %v, treating as empty", historyList["chats"], historyList["chats"])
		return []map[string]any{}, nil
	}

	chats := make([]map[string]any, 0, len(chatsRaw))
	for _, raw := range chatsRaw {
		switch v := raw.(type) {
		case string:
			var chat map[string]any
			if err := json.Unmarshal([]byte(v), &chat); err != nil {
				log.Printf("[m365-cloud] failed to parse chat string: %v", err)
				continue
			}
			chats = append(chats, chat)
		case map[string]any:
			chats = append(chats, v)
		default:
			log.Printf("[m365-cloud] unexpected chat type: %T", raw)
		}
	}

	return chats, nil
}

func (c *M365CloudClient) CleanupOldConversations(maxAge time.Duration, keepN int) (int, error) {
	// 微软历史列表是"滑动式"的：RefreshNavPane 一次只返回一屏对话，
	// 删除后进行到的对话会顶上来成为新一批。因此循环拉取删除，直到列表清空。
	now := time.Now().UnixMilli()
	deleted := 0
	kept := 0
	for round := 0; round < 100; round++ {
		chats, err := c.ListConversations()
		if err != nil {
			return deleted, err
		}
		if len(chats) == 0 {
			break
		}
		anyDeleted := false
		for _, chat := range chats {
			convID, _ := chat["conversationId"].(string)
			createTime, _ := chat["createTimeUtc"].(float64)
			if convID == "" {
				continue
			}

			age := time.Duration(now-int64(createTime)) * time.Millisecond
			if age > maxAge {
				if err := c.DeleteConversation(convID); err != nil {
					log.Printf("[m365-cloud] failed to delete %s: %v", convID, err)
					continue
				}
				deleted++
				anyDeleted = true
			} else {
				if kept >= keepN {
					if err := c.DeleteConversation(convID); err != nil {
						log.Printf("[m365-cloud] failed to delete %s: %v", convID, err)
						continue
					}
					deleted++
					anyDeleted = true
				} else {
					kept++
				}
			}
		}
		// 本轮没有任何删除（剩余的都是保留项），列表不会再变化，停止循环。
		if !anyDeleted {
			break
		}
	}

	log.Printf("[m365-cloud] cleanup: deleted %d, kept %d", deleted, kept)
	return deleted, nil
}

// stringReader 曾是个无游标的 io.Reader：每次 Read 都从字符串开头 copy，
// 当 payload 超过写入缓冲（io.Copy 默认 32KB）时会把前 32KB 反复发送。
// 已全部替换为 strings.NewReader，这里不再保留该类型。

// m365CloudPool 按账号持有云端对话客户端。云端对话属于创建它的账号，用别的
// 账号的 token 去删只会失败，因此列举与删除都必须走对应账号的客户端。
type m365CloudPool struct {
	mu      sync.Mutex
	clients map[string]*M365CloudClient
	build   func(accountID string) (*M365CloudClient, bool)
	order   func() []string
}

// Get 返回该账号的客户端，惰性创建。
func (p *m365CloudPool) Get(accountID string) (*M365CloudClient, bool) {
	if p == nil || accountID == "" {
		return nil, false
	}
	p.mu.Lock()
	if c, ok := p.clients[accountID]; ok {
		p.mu.Unlock()
		return c, true
	}
	p.mu.Unlock()

	c, ok := p.build(accountID)
	if !ok {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.clients[accountID]; ok {
		return existing, true
	}
	p.clients[accountID] = c
	return c, true
}

// Any 返回任意一个可用客户端，供不带账号信息的调用兜底。
func (p *m365CloudPool) Any() (*M365CloudClient, bool) {
	if p == nil {
		return nil, false
	}
	for _, id := range p.AccountIDs() {
		if c, ok := p.Get(id); ok {
			return c, true
		}
	}
	return nil, false
}

// AccountIDs 列出当前所有账号 ID，供需要遍历全部账号的清理逻辑使用。
func (p *m365CloudPool) AccountIDs() []string {
	if p == nil || p.order == nil {
		return nil
	}
	return p.order()
}

var m365CloudClients *m365CloudPool

// InitM365CloudPool 用账号存储装配客户端池：凭据每次现取，刷新后回写，
// 每个账号一个独立客户端。
func InitM365CloudPool(store *auth.Store, defaultClientID string) {
	m365CloudClients = &m365CloudPool{
		clients: map[string]*M365CloudClient{},
		order: func() []string {
			accounts := store.List()
			ids := make([]string, 0, len(accounts))
			for _, a := range accounts {
				ids = append(ids, a.ID)
			}
			return ids
		},
		build: func(accountID string) (*M365CloudClient, bool) {
			acc, ok := store.Get(accountID)
			if !ok {
				return nil, false
			}
			tid := acc.TID
			if tid == "" {
				if _, t := extractOIDTID(acc.AccessToken); t != "" {
					tid = t
				}
			}
			if tid == "" {
				log.Printf("[m365-cloud] account %s has no tenant id, cloud conversation API unavailable", acc.Email)
				return nil, false
			}
			clientID := firstNonEmpty(os.Getenv("M365_CLIENT_ID"), acc.ClientID, defaultClientID)
			id := accountID
			return NewM365CloudClientWithStore(clientID, tid,
				func() string {
					if cur, ok := store.Get(id); ok {
						return cur.RefreshToken
					}
					return ""
				},
				func(newToken string) {
					if err := store.UpdateRefreshToken(id, newToken); err != nil {
						log.Printf("[m365-cloud] persist rotated refresh token for %s failed: %v", id, err)
					}
				},
			), true
		},
	}
	log.Printf("[m365-cloud] client pool initialized for %d account(s)", len(m365CloudClients.AccountIDs()))
}

// GetM365CloudClient 返回任意可用客户端，仅供无账号上下文的旧调用点使用。
func GetM365CloudClient() *M365CloudClient {
	c, _ := m365CloudClients.Any()
	return c
}

// cloudClientFor 返回指定账号的客户端；账号未知时退回任意可用客户端。
func cloudClientFor(accountID string) (*M365CloudClient, bool) {
	if c, ok := m365CloudClients.Get(accountID); ok {
		return c, true
	}
	return m365CloudClients.Any()
}

// cloudConfigured 表示至少有一个账号可以操作云端对话。
func cloudConfigured() bool {
	_, ok := m365CloudClients.Any()
	return ok
}
