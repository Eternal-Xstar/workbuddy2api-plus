package panel

import (
	"net/http"
	"strconv"
	"strings"
)

// errorSummary GET /panel/api/error_summary[?hours=24]
//
// 上游错误的近因聚合（面板顶部红标 + wbapi.ps1 doctor 共用）。
// 只输出元数据：业务码、分类、次数、首末时间、涉及账号与模型、一句人话提示。
func (p *Panel) errorSummary(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ErrorSummary == nil {
		writeErr(w, http.StatusNotImplemented, "error summary unavailable")
		return
	}
	hours := 24
	if v := strings.TrimSpace(r.URL.Query().Get("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*30 {
			hours = n
		}
	}
	writeJSON(w, http.StatusOK, p.cfg.ErrorSummary(hours))
}