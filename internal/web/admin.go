package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"cybercompanion/internal/api"
	// 用别名导入：本包内已有一个名为 store 的会话存储变量（auth.go），
	// 直接 import 会造成同名遮蔽，读代码时极易误判。
	datastore "cybercompanion/internal/store"
)

// ─── 控制台管理端点 ──────────────────────────────────────────────────────────
//
// 原面板只能看硬件状态、改配置、切人设、读日志 —— 数据库带来的用户、记忆、
// 统计、插件、调度这些新东西，用户既看不到也管不了（优化建议书第十四节）。
//
// 权限边界：这些端点全部挂在 requireAuth 之后（与其余 /api/* 一致），
// 也就是「能登录面板」才能访问。面板密码与主人口令是两套东西：
// 前者守住本机的管理界面，后者守住聊天里的设备控制权。

// maxAdminBody 限制管理端点的请求体大小。
//
// 这些接口的入参都是小 JSON，1MB 已经极其宽松；
// 设上限是为了避免一个恶意/错误的请求把内存撑爆。
const maxAdminBody = 1 << 20

// registerAdminRoutes 注册管理端点。admin 为 nil 时全部返回 503。
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.Handle("/api/admin/users", requireAuth(http.HandlerFunc(s.handleAdminUsers)))
	mux.Handle("/api/admin/users/role", requireAuth(http.HandlerFunc(s.handleAdminUserRole)))
	mux.Handle("/api/admin/memories", requireAuth(http.HandlerFunc(s.handleAdminMemories)))
	mux.Handle("/api/admin/memory/owners", requireAuth(http.HandlerFunc(s.handleAdminMemoryOwners)))
	mux.Handle("/api/admin/usage", requireAuth(http.HandlerFunc(s.handleAdminUsage)))
	mux.Handle("/api/admin/plugins", requireAuth(http.HandlerFunc(s.handleAdminPlugins)))
	mux.Handle("/api/admin/schedules", requireAuth(http.HandlerFunc(s.handleAdminSchedules)))
	mux.Handle("/api/admin/schedules/toggle", requireAuth(http.HandlerFunc(s.handleAdminScheduleToggle)))
	mux.Handle("/api/admin/debug", requireAuth(http.HandlerFunc(s.handleAdminDebug)))
}

// adminUnavailable 在管理能力缺失（数据库不可用）时统一应答。
func adminUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "持久层不可用，此功能需要数据库支持",
	})
}

// writeJSONError 输出错误响应。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// decodeBody 解析请求体，带大小限制。
func decodeBody(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, maxAdminBody)).Decode(v)
}

// queryInt 读取整数查询参数。
func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// handleAdminUsers 列出用户，或修改某人的权限档位。
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	users, counts, err := s.admin.ListUsers(queryInt(r, "limit", 100), queryInt(r, "offset", 0))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 档位人数转成字符串键，便于前端直接索引
	roleCounts := map[string]int{}
	for role, n := range counts {
		roleCounts[string(role)] = n
	}

	writeJSON(w, map[string]interface{}{
		"users":       users,
		"role_counts": roleCounts,
	})
}

// handleAdminUserRole 修改用户权限档位。
func (s *Server) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		OpenID string `json:"openid"`
		Role   string `json:"role"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if strings.TrimSpace(req.OpenID) == "" {
		writeJSONError(w, http.StatusBadRequest, "缺少 openid")
		return
	}

	changed, err := s.admin.SetUserRole(req.OpenID, req.Role)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "changed": changed})
}

// handleAdminMemories 列出 / 检索 / 新增 / 删除记忆。
func (s *Server) handleAdminMemories(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		scope := r.URL.Query().Get("scope")
		owner := r.URL.Query().Get("owner")
		if owner == "" {
			writeJSONError(w, http.StatusBadRequest, "缺少 owner 参数")
			return
		}
		items, total, err := s.admin.ListMemories(scope, owner,
			r.URL.Query().Get("q"), queryInt(r, "limit", 100), queryInt(r, "offset", 0))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"memories": items, "total": total})

	case http.MethodPost:
		var req struct {
			Scope      string  `json:"scope"`
			Owner      string  `json:"owner"`
			Content    string  `json:"content"`
			Category   string  `json:"category"`
			Importance float64 `json:"importance"`
		}
		if err := decodeBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		if strings.TrimSpace(req.Content) == "" || req.Owner == "" {
			writeJSONError(w, http.StatusBadRequest, "缺少内容或归属者")
			return
		}
		id, err := s.admin.AddMemory(req.Scope, req.Owner, req.Content, req.Category, req.Importance)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "id": id})

	case http.MethodDelete:
		id := int64(queryInt(r, "id", 0))
		if id <= 0 {
			writeJSONError(w, http.StatusBadRequest, "缺少有效的 id")
			return
		}
		if err := s.admin.DeleteMemory(id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminMemoryOwners 列出有记忆的对话边界。
func (s *Server) handleAdminMemoryOwners(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	owners, err := s.admin.MemoryOwnerList()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"owners": owners})
}

// handleAdminUsage 返回模型用量统计。
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	overview, err := s.admin.UsageStats(queryInt(r, "days", 14))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, overview)
}

// handleAdminPlugins 列出插件或切换其启用状态。
func (s *Server) handleAdminPlugins(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		list, err := s.admin.ListPlugins()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"plugins": list})

	case http.MethodPost:
		var req struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := decodeBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		if req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "缺少插件名")
			return
		}
		if err := s.admin.SetPluginEnabled(req.Name, req.Enabled); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminSchedules 列出 / 新增 / 删除调度任务。
func (s *Server) handleAdminSchedules(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		list, err := s.admin.ListSchedules(r.URL.Query().Get("owner"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if list == nil {
			list = []datastore.Schedule{}
		}
		writeJSON(w, map[string]interface{}{"schedules": list})

	case http.MethodPost:
		var req api.ScheduleRequest
		if err := decodeBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		id, err := s.admin.SaveSchedule(req)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "id": id})

	case http.MethodDelete:
		id := int64(queryInt(r, "id", 0))
		if id <= 0 {
			writeJSONError(w, http.StatusBadRequest, "缺少有效的 id")
			return
		}
		if err := s.admin.DeleteSchedule(id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminScheduleToggle 启用或停用一条调度任务。
func (s *Server) handleAdminScheduleToggle(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.ID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "缺少有效的 id")
		return
	}
	if err := s.admin.SetScheduleEnabled(req.ID, req.Enabled); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleAdminDebug 返回内部状态快照。
func (s *Server) handleAdminDebug(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		adminUnavailable(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	info, err := s.admin.Debug()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, info)
}
