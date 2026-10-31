package appkit

import (
	"net/http"
	"strconv"
	"strings"
)

// The kit's error answers are translated for the caller's locale the way the platform's are (internal/i18n): Err answers a known
// message in Chinese for a Chinese caller and in English otherwise, and leaves any other message as it is. The catalog is the
// platform's, copied (an app on its own answers its refusals in the same words as in the platform's process).

const (
	localeZH      = "zh"
	localeEN      = "en"
	defaultLocale = localeEN
)

func normalizeLocale(tag string) string {
	s := strings.ToLower(strings.TrimSpace(tag))
	if strings.HasPrefix(s, "zh") {
		return localeZH
	}
	if strings.HasPrefix(s, "en") {
		return localeEN
	}
	return defaultLocale
}

// parseAcceptLanguage is the best supported locale of an Accept-Language header (by quality; the first of equal ones).
func parseAcceptLanguage(header string) string {
	if header == "" {
		return defaultLocale
	}
	best, bestQ := "", 0.0
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		params := strings.Split(part, ";")
		lower := strings.ToLower(strings.TrimSpace(params[0]))
		q := 1.0
		for _, p := range params[1:] {
			if v, ok := strings.CutPrefix(strings.TrimSpace(p), "q="); ok {
				if parsed, err := strconv.ParseFloat(v, 64); err == nil {
					q = parsed
				}
			}
		}
		lang := ""
		if strings.HasPrefix(lower, "zh") {
			lang = localeZH
		} else if strings.HasPrefix(lower, "en") {
			lang = localeEN
		}
		if lang != "" && q > 0 && (best == "" || q > bestQ) {
			best, bestQ = lang, q
		}
	}
	if best == "" {
		return defaultLocale
	}
	return best
}

func localeFromRequest(r *http.Request) string {
	if r == nil {
		return defaultLocale
	}
	if h := r.Header.Get("Accept-Language"); h != "" {
		return parseAcceptLanguage(h)
	}
	return defaultLocale
}

// localeFromWriter is the locale of the request a writer carries (ReqWriter), else the response's Content-Language.
func localeFromWriter(w http.ResponseWriter) string {
	if rc, ok := w.(interface{ Request() *http.Request }); ok && rc != nil {
		return localeFromRequest(rc.Request())
	}
	if w != nil {
		if h := w.Header().Get("Content-Language"); h != "" {
			return normalizeLocale(h)
		}
	}
	return defaultLocale
}

func translate(loc, msg string) string {
	if msg == "" {
		return ""
	}
	if normalizeLocale(loc) == localeZH {
		if zh, ok := enToZh[msg]; ok {
			return zh
		}
		if zh, ok := enToZh[strings.ToLower(msg)]; ok {
			return zh
		}
		return msg
	}
	if en, ok := zhToEn[msg]; ok {
		return en
	}
	return msg
}

var (
	// enToZh maps English message / key to Chinese.
	enToZh = map[string]string{
		"not found": "未找到",
		"not found, or you lack permission to view": "没有找到，或你没有权限查看",
		"you are not allowed to do this":            "无权执行此操作",
		"your role does not permit this":            "你的角色不允许这样做",
		"sign in through the workspace":             "请通过工作台登录",
		"the agent only reads":                      "助手仅支持读取操作",
		"already exists":                            "已存在",
		"already exists with the same name":         "已经存在同名的记录",
		"conflict":                                  "发生冲突",
		"invalid":                                   "参数无效",
		"internal error":                            "内部错误",
		"the request is too large":                  "请求体过大",
		"invalid JSON":                              "无效的 JSON",
		"too many requests":                         "请求过于频繁",
		"service unavailable":                       "服务暂不可用",
		"database not connected":                    "没有连接数据库",
		"bad request":                               "请求错误",
		"forbidden":                                 "禁止访问",
		"unauthorized":                              "未登录",
		"organization":                              "我的组织",
		"messages":                                  "消息中心",
		"agent drafts":                              "Agent 草稿",
		"scheduled tasks":                           "定时任务",
	}

	// zhToEn maps Chinese message to English.
	zhToEn = map[string]string{
		"未找到":  "not found",
		"没有找到": "not found",
		"没有找到，或你没有权限查看":     "not found, or you lack permission to view",
		"找不到这条记录，或者你没有权限查看": "record not found, or you lack permission to view",
		"无权执行此操作":           "you are not allowed to do this",
		"你的角色不允许这样做":        "your role does not permit this action",
		"请通过工作台登录":          "sign in through the workspace",
		"助手仅支持读取操作":         "the agent only reads",
		"已存在":               "already exists",
		"已经存在同名的记录":         "record with this name already exists",
		"发生冲突":              "conflict",
		"冲突":                "conflict",
		"参数无效":              "invalid",
		"无效":                "invalid",
		"内部错误":              "internal error",
		"请求体过大":             "request body is too large",
		"请求太大":              "request is too large",
		"无效的 JSON":          "invalid JSON",
		"请求不是有效的 JSON":      "request body is not valid JSON",
		"请求过于频繁":            "too many requests",
		"服务暂不可用":            "service temporarily unavailable",
		"请求错误":              "bad request",
		"禁止访问":              "forbidden",
		"未登录":               "unauthorized",
		"剩余次数不足":            "insufficient remaining credits",
		"库存不足":              "insufficient inventory",
		"密码不正确":             "incorrect password",
		"需要密码":              "password required",
		"我的组织":              "Organization",
		"组织结构与权限、应用、助手、数据、品牌、安全与审计": "Org structure and access, apps, assistant, data, brand, security, and audit",
		"消息中心": "Messages",
		"站内信、平台通知和系统提醒": "Inbox, platform notices, and system alerts",
		"Agent 草稿": "Agent drafts",
		"管理定时任务；审阅、修改并应用助手起草的变更；管理自动应用；查看和整理会话": "Manage timed runs; review, edit, and apply what the assistant drafted; manage auto-apply; browse and tidy conversations",
		"定时任务": "Scheduled tasks",
		"录音":   "Recordings",
		"消息":   "Messages",
		"今日":   "Today",
		"日程":   "Calendar",
		"待办":   "To do",
		"今日、日程和待办：各应用的卡片与等你处理的事": "Today, calendar and to-dos: the apps' cards and what waits on you",
		"草稿":   "Drafts",
		"自动应用": "Auto-apply",
		"行政":   "Administration",
		"办公":   "Work",
		"业务数据": "Business data",
		"组织自己的数据类型和记录：客户、合同、工单……按你的权限查看、新建和修改": "The organization’s own data types and records (customers, contracts, tickets…): view, create and change what your permissions allow",
		"数据分析":    "Analytics",
		"看板":      "Boards",
		"探索":      "Explore",
		"数据":      "Data",
		"没有连接数据库": "database not connected",
	}
)
