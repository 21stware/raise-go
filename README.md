# raise-go

Raise 开放接口的 Go SDK。令牌、重试、幂等键和分页都由客户端代劳。

## 安装

```go
import "github.com/21stware/raise-go"
```

```
go get github.com/21stware/raise-go
```

## 换令牌

```go
c := raise.New(os.Getenv("RAISE_CLIENT_ID"), os.Getenv("RAISE_CLIENT_SECRET"),
    raise.WithBaseURL(os.Getenv("RAISE_BASE_URL")), // https://api.example.com/open/v1
    raise.WithTenant(os.Getenv("RAISE_TENANT")),    // 商店应用必填
)
```

第一次调用时会 `POST /oauth/token`。令牌缓存按组织，到期前 60 秒自动换。

## 第一次调用

```go
me, err := c.Platform.Whoami(ctx)
if err != nil {
    log.Fatal(err)
}
fmt.Println(me.Tenant, me.App, me.Scopes)
```

## 分页

```go
it := c.Tables.ListRowsAll(ctx, "orders", "all", nil)
for it.Next(ctx) {
    fmt.Println(it.Item().ID)
}
if err := it.Err(); err != nil {
    log.Fatal(err)
}
```

## 错误

```go
if err != nil {
    var e *raise.Error
    if errors.As(err, &e) && raise.IsCode(err, "scope_required") {
        log.Println("去开发者平台勾范围", e.Message, e.RequestID)
    }
}
```

## 向开发者平台报告应用的错误

`POST /app/v1/errors`（manifest 的 `uses` 声明 `app.errors:report`）。平台在保存前脱敏：开发者平台和 `k3 logs --errors` 只看到占位符，看不到企业的数据。自由文本 `Message` 里的值（引号内容、数字、邮箱、网址、令牌、`key=value`）和**所有中文等非 ASCII 文字**都换成占位符（`<str>` `<n>` `<email>` `<text>`…）。想让开发者读得懂：报 `Code` + 固定的 `Template`（代码里的常量，原文保留）+ `Params`（值，不进开发者视图，只在企业批准的限时诊断里给开发者看）。

```go
const errMemberMissing = "会员不存在：{name}" // 常量，不拼值
_, err := c.App.Errors.Report(ctx, &raise.AppErrorReport{Errors: []raise.AppErrorOccurrence{{
    Kind: "NotFound", Code: "member.missing", Template: errMemberMissing, Params: []string{name},
    Frames: []string{"members.get (members.go:42)"}, Route: "/api/members/{id}", Method: "GET", Version: "1.4.0",
}}})
```

## Webhook

```go
ev, err := raise.VerifyWebhook(body, r.Header.Get("X-Kwork-Signature"), secret, 5*time.Minute)
if err != nil {
    http.Error(w, "bad signature", 400)
    return
}
log.Println(ev.Type, ev.ID)
```

## 测试与正式

密钥前缀决定环境：`kwo_test_` 只能打测试企业，`kwo_live_`（以及旧的 `kwo_`）打正式组织。客户端不猜环境，平台用 `wrong_environment` 拒绝打错的一边。例子见 `examples/quickstart` 和 `examples/webhook`。
