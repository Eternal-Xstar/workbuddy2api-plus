// accountjson.go 账号 JSON 的解析与导出（面板「导入 / 导出」）。
//
// 为什么单独成文件：导入要容忍**多种真实世界形态**，导出要能**回灌给自己**。
// 这两件事都是纯数据问题，与 HTTP 编排无关，放一起便于测。
//
// 导入侧承认的形态（都来自实际遇到的文件）：
//
//	1) 数组：            [ {account}, {account} ]
//	2) 信封 + 数组：      {"accounts|data|list|items|results|auths": [ ... ]}
//	3) 信封 + 单对象：    {"accounts": {account}}       （导出工具套了一层）
//	4) 单账号对象：       {account}
//	5) 官方客户端文件：   {"auth": {...}, "account": {...}}   ← workbuddy-desktop.info 形态
//	6) 字典：            {"<uid>": {account}, ...}     （键当 uid 兜底）
//
// 账号字段同时认 snake_case 与 camelCase（access_token / accessToken），
// 时间戳毫秒秒都能吃（>1e12 视为毫秒）。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Eternal-Xstar/workbuddy2api-plus/internal/auth"
)

// accountRaw 一个账号在 JSON 里的原始字段（宽松解析：两种命名都收）。
type accountRaw struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Email      string `json:"email"`
	Domain     string `json:"domain"`
	Realm      string `json:"realm"`
	AccessTok  string `json:"access_token"`
	AccessCml  string `json:"accessToken"`
	RefreshTok string `json:"refresh_token"`
	RefreshCml string `json:"refreshToken"`
	ExpiresAt  int64  `json:"expires_at"`
	ExpiresCml int64  `json:"expiresAt"`
	Enterprise string `json:"enterprise_id"`
	EnterCml   string `json:"enterpriseId"`

	// 官方客户端文件的两段式形态：{"auth":{...},"account":{...}}
	Auth    *accountRaw `json:"auth"`
	Account *accountRaw `json:"account"`
}

// normalize 把宽松字段收敛成一份，并合并嵌套两段式。
func (r accountRaw) normalize() cockpitAccount {
	out := cockpitAccount{
		UID:          strings.TrimSpace(r.UID),
		Nickname:     strings.TrimSpace(r.Nickname),
		Email:        strings.TrimSpace(r.Email),
		Domain:       strings.TrimSpace(r.Domain),
		Realm:        strings.TrimSpace(r.Realm),
		AccessToken:  firstNonEmpty(r.AccessTok, r.AccessCml),
		RefreshToken: firstNonEmpty(r.RefreshTok, r.RefreshCml),
		ExpiresAt:    firstNonZero(r.ExpiresAt, r.ExpiresCml),
		EnterpriseID: firstNonEmpty(r.Enterprise, r.EnterCml),
	}
	// workbuddy-desktop.info：认证在 auth 段、身份在 account 段。
	if a := r.Auth; a != nil {
		out.AccessToken = firstNonEmpty(out.AccessToken, a.AccessTok, a.AccessCml)
		out.RefreshToken = firstNonEmpty(out.RefreshToken, a.RefreshTok, a.RefreshCml)
		out.ExpiresAt = firstNonZero(out.ExpiresAt, a.ExpiresAt, a.ExpiresCml)
		out.Domain = firstNonEmpty(out.Domain, a.Domain)
		out.Realm = firstNonEmpty(out.Realm, a.Realm)
	}
	if ac := r.Account; ac != nil {
		out.UID = firstNonEmpty(out.UID, ac.UID)
		out.Nickname = firstNonEmpty(out.Nickname, ac.Nickname)
		out.Email = firstNonEmpty(out.Email, ac.Email)
		out.EnterpriseID = firstNonEmpty(out.EnterpriseID, ac.Enterprise, ac.EnterCml)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// accountEnvelopeKeys 导出工具常见的信封键（**不是 uid**）。
var accountEnvelopeKeys = []string{"accounts", "data", "list", "items", "results", "auths"}

func isAccountEnvelopeKey(k string) bool {
	for _, e := range accountEnvelopeKeys {
		if strings.EqualFold(k, e) {
			return true
		}
	}
	return false
}

// decodeAccountsJSON 把任意受支持形态解码成账号列表；认不出就报错（不猜）。
func decodeAccountsJSON(raw []byte) ([]cockpitAccount, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		return decodeAccountItems(items), nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		// 信封：值是数组 → 逐条解；值是单个对象 → 按单账号解。
		for _, k := range accountEnvelopeKeys {
			sub, ok := obj[k]
			if !ok {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal(sub, &items); err == nil {
				return decodeAccountItems(items), nil
			}
			if acc, ok := decodeAccountOne(sub); ok {
				return []cockpitAccount{acc}, nil
			}
		}
		// 官方客户端两段式 / 单账号对象。
		if _, ok := obj["auth"]; ok {
			return decodeAccountItems([]json.RawMessage{trimmed}), nil
		}
		if _, ok := obj["account"]; ok {
			return decodeAccountItems([]json.RawMessage{trimmed}), nil
		}
		if acc, ok := decodeAccountOne(trimmed); ok {
			return []cockpitAccount{acc}, nil
		}
		// 字典：{"<uid>": {...}}。信封名不能当 uid（否则会造出 uid="accounts" 的垃圾号）。
		keys := make([]string, 0, len(obj))
		for k := range obj {
			if isAccountEnvelopeKey(k) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]cockpitAccount, 0, len(keys))
		for _, k := range keys {
			acc, ok := decodeAccountOne(obj[k])
			if !ok {
				continue
			}
			if acc.UID == "" {
				acc.UID = k
			}
			out = append(out, acc)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("无法识别的 JSON 结构：既不是账号数组，也不是 {account,auth} 或 {\"<uid>\":{…}}")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected JSON array or object")
	}
}

// decodeAccountItems 逐条解，跳过解不出的条目（由调用方按 skipped 计数报出）。
func decodeAccountItems(items []json.RawMessage) []cockpitAccount {
	out := make([]cockpitAccount, 0, len(items))
	for _, it := range items {
		if acc, ok := decodeAccountOne(it); ok {
			out = append(out, acc)
		}
	}
	return out
}

// decodeAccountOne 解单条；既没有 uid 也没有 token 的条目视为"不是账号"。
func decodeAccountOne(raw json.RawMessage) (cockpitAccount, bool) {
	var r accountRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return cockpitAccount{}, false
	}
	acc := r.normalize()
	if acc.UID == "" && acc.AccessToken == "" {
		return cockpitAccount{}, false
	}
	// 毫秒时间戳（cockpit / 官方客户端导出都是 ms）→ 秒；已是秒的原样保留。
	if acc.ExpiresAt > 1e12 {
		acc.ExpiresAt /= 1000
	}
	if acc.ExpiresAt > 0 && acc.ExpiresAt < 1e9 {
		// 明显不是合法秒级时间戳（< 2001 年）：置零，由导入侧给默认值。
		acc.ExpiresAt = 0
	}
	return acc, true
}

// ---- 导出 ----

// exportAccount 导出用的单条账号（字段名与导入侧完全对称，保证回灌可用）。
type exportAccount struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Realm        string `json:"realm,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒（与 cockpit / 官方客户端一致）
}

// exportAccounts 把池内账号导出成可回灌的 JSON 数组。redact=true 时剔除 token
// （用于贴到 issue / 分享账号清单，不含任何凭据）。
func (p *Panel) exportAccounts(redact bool) ([]exportAccount, error) {
	accts := p.cfg.Pool.List()
	out := make([]exportAccount, 0, len(accts))
	for _, st := range accts {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		item := exportAccount{
			UID:          a.UID,
			Nickname:     a.Nickname,
			Domain:       a.DomainValue(),
			Realm:        a.Realm(),
			EnterpriseID: a.EnterpriseID,
			AccessToken:  a.AccessTokenValue(),
			RefreshToken: a.RefreshTokenValue(),
			ExpiresAt:    a.ExpiresAt * 1000,
		}
		if redact {
			item.AccessToken = ""
			item.RefreshToken = ""
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// accountsExport GET /panel/api/accounts/export[?redact=1]
//
// 默认整包（含 token）——这是"换机器/备份"的用途；redact=1 只给元数据。
// 文件名带日期，便于归档。
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	redact := r.URL.Query().Get("redact")
	doRedact := redact == "1" || strings.EqualFold(redact, "true")
	list, err := p.exportAccounts(doRedact)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	name := fmt.Sprintf("wb2api-accounts-%s.json", time.Now().Format("20060102-150405"))
	if doRedact {
		name = fmt.Sprintf("wb2api-accounts-redacted-%s.json", time.Now().Format("20060102-150405"))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// ensureAuthDir 导入前把凭证目录建好（全新部署时 auths/ 还不存在，写盘会 ENOENT）。
func ensureAuthDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// accountFilePath 账号凭证文件名（与登录/导入同口径，便于"是否已存在"判定）。
func accountFilePath(dir, uid string) string {
	return filepath.Join(dir, fmt.Sprintf("workbuddy-%s.json", uid))
}

// exportedAtHint 仅供日志：解析结果里有多少条带 token（不含内容）。
func countWithTokens(list []cockpitAccount) int {
	n := 0
	for _, a := range list {
		if a.AccessToken != "" && a.RefreshToken != "" {
			n++
		}
	}
	return n
}

var _ = auth.ResolveRealm // 保持与本包其它文件的依赖显式（导入用 realm 推断）
