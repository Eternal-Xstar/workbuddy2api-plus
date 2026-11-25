package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 六种真实形态都要能解出来（对齐社区分支的兼容矩阵）。
func TestDecodeAccountsJSONShapes(t *testing.T) {
	one := `{"uid":"u1","access_token":"a1","refresh_token":"r1","expires_at":1790000000000}`
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"数组", `[` + one + `,{"uid":"u2","access_token":"a2","refresh_token":"r2"}]`, 2},
		{"信封+数组", `{"accounts":[` + one + `]}`, 1},
		{"信封 data + 数组", `{"data":[` + one + `]}`, 1},
		{"信封+单对象", `{"accounts":` + one + `}`, 1},
		{"单账号对象", one, 1},
		{"官方两段式", `{"auth":{"access_token":"a9","refresh_token":"r9"},"account":{"uid":"u9","nickname":"九号"}}`, 1},
		{"字典", `{"u7":` + one + `,"u8":{"access_token":"a8","refresh_token":"r8"}}`, 2},
		{"驼峰字段", `{"uid":"u3","accessToken":"a3","refreshToken":"r3","expiresAt":1790000000000}`, 1},
		{"数组内两段式", `[{"account":{"uid":"u4"},"auth":{"accessToken":"a4","refreshToken":"r4","expiresAt":1795097981}}]`, 1},
	}
	for _, c := range cases {
		got, err := decodeAccountsJSON([]byte(c.raw))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Fatalf("%s: 解出 %d 条，期望 %d 条", c.name, len(got), c.want)
		}
	}
}

// 信封名不能当 uid（否则会造出 uid="accounts" 的垃圾账号）。
func TestDecodeAccountsJSONEnvelopeNotUID(t *testing.T) {
	got, err := decodeAccountsJSON([]byte(`{"accounts":{"access_token":"a","refresh_token":"r"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("条数 = %d", len(got))
	}
	if got[0].UID == "accounts" {
		t.Fatal("信封名被当成了 uid")
	}
	if got[0].UID != "" {
		t.Fatalf("这个形态没有 uid，应为空（由导入侧判缺字段），实得 %q", got[0].UID)
	}
}

// 时间戳：毫秒归一成秒；非法值置零由导入侧兜底。
func TestDecodeAccountsJSONExpires(t *testing.T) {
	got, _ := decodeAccountsJSON([]byte(`[{"uid":"u","access_token":"a","refresh_token":"r","expires_at":1790000000000}]`))
	if got[0].ExpiresAt != 1790000000 {
		t.Fatalf("毫秒没归一: %d", got[0].ExpiresAt)
	}
	got2, _ := decodeAccountsJSON([]byte(`[{"uid":"u","access_token":"a","refresh_token":"r","expires_at":123}]`))
	if got2[0].ExpiresAt != 0 {
		t.Fatalf("非法时间戳应置零: %d", got2[0].ExpiresAt)
	}
}

// 认不出的结构要报错，不能静默返回空。
func TestDecodeAccountsJSONRejects(t *testing.T) {
	for _, raw := range []string{``, `  `, `"str"`, `123`, `{}`, `{"foo":"bar"}`} {
		if _, err := decodeAccountsJSON([]byte(raw)); err == nil {
			t.Fatalf("%q 应该报错", raw)
		}
	}
}

// 导出的形状必须能被自己的导入器读回（往返一致），字段对称。
func TestExportShapeRoundTrip(t *testing.T) {
	list := []exportAccount{{
		UID: "u1", Nickname: "一号", Domain: "www.codebuddy.cn", Realm: "cn",
		AccessToken: "a1", RefreshToken: "r1", ExpiresAt: 1790000000000,
	}}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeAccountsJSON(raw)
	if err != nil {
		t.Fatalf("导出物无法被导入器解析: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("条数 = %d", len(back))
	}
	b := back[0]
	if b.UID != "u1" || b.AccessToken != "a1" || b.RefreshToken != "r1" {
		t.Fatalf("往返丢字段: %+v", b)
	}
	if b.ExpiresAt != 1790000000 {
		t.Fatalf("往返时间戳 = %d（应归一成秒）", b.ExpiresAt)
	}
	if b.Realm != "cn" {
		t.Fatalf("往返丢了 realm: %+v", b)
	}
}

// redact 版本不得包含任何凭据字符串。
func TestExportRedactHasNoTokens(t *testing.T) {
	raw, _ := json.Marshal([]exportAccount{{UID: "u1", Realm: "cn"}})
	if strings.Contains(string(raw), "token") {
		t.Fatalf("redact 形态仍含 token 字段: %s", raw)
	}
}

// 全新部署（auths/ 还不存在）时导入必须能把目录建出来，否则写盘 ENOENT。
func TestEnsureAuthDirCreatesMissing(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "auths", "nested")
	if err := ensureAuthDir(dir); err != nil {
		t.Fatalf("ensureAuthDir: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("目录没建出来: %v", err)
	}
	// 幂等：已存在时再调不报错。
	if err := ensureAuthDir(dir); err != nil {
		t.Fatalf("重复调用应幂等: %v", err)
	}
	if got := accountFilePath(dir, "u1"); !strings.HasSuffix(got, "workbuddy-u1.json") {
		t.Fatalf("凭证文件名口径不对: %s", got)
	}
}