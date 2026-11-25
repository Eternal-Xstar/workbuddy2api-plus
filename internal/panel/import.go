package panel

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	TokenType     string `json:"token_type"`
	ExpiresAt     int64  `json:"expires_at"`
	Domain        string `json:"domain"`
	DosageNotify  string `json:"dosage_notify_code"`
	PaymentType   string `json:"payment_type"`
	Status        string `json:"status"`
	UsageUpdatedAt int64 `json:"usage_updated_at"`
	LastCheckin   int64  `json:"last_checkin_time"`
	CheckinStreak int    `json:"checkin_streak"`
	CreatedAt     int64  `json:"created_at"`
	LastUsed      int64  `json:"last_used"`
	// Realm 导出文件里可能显式写 cn/global；EnterpriseID 企业版必需
	// （决定 X-Private-Data、企业额度口径与签到门控），此前导入会把它丢掉。
	Realm        string `json:"realm"`
	EnterpriseID string `json:"enterprise_id"`
}

// importCockpit 批量导入账号。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>（可重复，按顺序逐个解码）
//	      dryRun=true|1 → 只校验并预览：不落盘、不上游（不签到/不查额度）
//
// 多个文件是**整体事务**：任一文件不是合法 JSON 或没有任何账号就整体 400 ——
// 导入不可逆，"先导进去一半"是最难收拾的状态。
//
// 返回 {ok, total, imported, skipped, creates, overwrites, dry_run, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	dryRun := strings.EqualFold(strings.TrimSpace(r.FormValue("dryRun")), "true") ||
		strings.TrimSpace(r.FormValue("dryRun")) == "1"

	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}

	var accounts []cockpitAccount
	var sources []string
	for _, fh := range headers {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "open "+fh.Filename+": "+err.Error())
			return
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": read file: "+err.Error())
			return
		}
		list, err := decodeAccountsJSON(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": "+err.Error())
			return
		}
		if len(list) == 0 {
			writeErr(w, http.StatusBadRequest, fh.Filename+": 文件里没有任何账号条目")
			return
		}
		for _, acc := range list {
			accounts = append(accounts, acc)
			sources = append(sources, fh.Filename)
		}
	}

	// 凭证目录是进程级前置条件：全新部署（auths/ 还没建）时写盘会 ENOENT。
	// 预览不建任何东西。
	if !dryRun {
		if err := ensureAuthDir(p.cfg.AuthDir); err != nil {
			writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
			return
		}
	}

	var imported, skipped, creates, overwrites int
	var errs []string
	seen := map[string]string{} // uid → 首次出现的文件（批内去重）

	for i, acc := range accounts {
		src := ""
		if i < len(sources) {
			src = sources[i]
		}
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: 缺少 uid/access_token/refresh_token（id=%s）", src, acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: uid 不合法 %q", src, uid))
			continue
		}
		if prev, dup := seen[uid]; dup {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: uid=%s 在本次导入中重复（已在 %s 出现）", src, uid, prev))
			continue
		}
		seen[uid] = src

		// realm：显式 realm 优先，其次按 domain 推断（workbuddy.ai 家族 → global）。
		realm := strings.TrimSpace(acc.Realm)
		if realm != "global" && realm != "cn" {
			realm = auth.ResolveRealm("", acc.Domain)
		}

		// expires_at：解码层已归一到秒；仍为空/过旧 → 给一年有效期（导入后会用 refresh 换新）。
		expiresAt := acc.ExpiresAt
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		path := accountFilePath(p.cfg.AuthDir, uid)
		_, statErr := os.Stat(path)
		exists := statErr == nil
		if exists {
			overwrites++
		} else {
			creates++
		}
		if dryRun {
			continue
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			EnterpriseID: acc.EnterpriseID,
			FilePath:     path,
		}
		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				overwrites++
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}
		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}
		imported++
	}

	total := len(accounts)
	if dryRun {
		log.Printf("panel: import dry-run total=%d creates=%d overwrites=%d skipped=%d",
			total, creates, overwrites, skipped)
	} else {
		log.Printf("panel: import finished total=%d imported=%d skipped=%d creates=%d overwrites=%d",
			total, imported, skipped, creates, overwrites)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"dry_run":    dryRun,
		"total":      total,
		"imported":   imported,
		"skipped":    skipped,
		"creates":    creates,
		"overwrites": overwrites,
		"with_token": countWithTokens(accounts),
		"errors":     errs,
	})
}
// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
