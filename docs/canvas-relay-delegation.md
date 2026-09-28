# 影策单任务 Relay 委托合同（受限实现）

此接口仅供配置的影策服务端调用。所有请求使用 `X-Canvas-Client-Secret`，响应 `Cache-Control: no-store`；不得从浏览器调用或记录凭据。需要配置已有身份交换使用的 `NEWAPI_CANVAS_CLIENT_ID`、`NEWAPI_CANVAS_CLIENT_SECRET`（至少 32 字节）、`NEWAPI_CANVAS_REDIRECT_URI` 和 `NEWAPI_CANVAS_ISSUER`。目前不启用影策侧模型调用切换，也不移除影策的 503 拒绝。

## 授权（不扣款）

`POST /api/internal/integrations/canvas/v1/relay/authorizations`，加 `Idempotency-Key`（16–128 字节）。JSON：

```json
{
  "user_id": 42,
  "task_id": "canvas-task-0001",
  "plugin_key": "video",
  "model": "task-model",
  "payload_sha256": "64位小写十六进制SHA-256",
  "max_quota": 100,
  "expires_at": "<当前UTC时间后60秒的RFC3339时间>"
}
```

服务端必须配置正整数 `NEWAPI_CANVAS_MAX_QUOTA`，客户端上限不得超过它。`expires_at` 按 UTC 秒精度截断后必须在服务端当前时间后 5 秒至 5 分钟内。`payload_sha256` 是下一步完整原始 JSON 请求体的摘要，不是模型输入的摘要。相同 client + key + 全部字段返回同一授权；同一 client/task 的不同请求或 key 冲突返回 409。首次响应为 `201`，相同授权重取为 `200`，包含 `id`、`state`、`expires_at`；未领取时还有 `delegation_secret`。授权只创建有额度上限、模型限制和到期时间的内部原生 Token；**此时不预扣用户额度**。

## 单次提交

`POST /api/internal/integrations/canvas/v1/relay/authorizations/:id/tasks/:key`：加 `X-Canvas-User-ID`（与授权用户相同）、`X-Canvas-Delegation`（授权响应凭据）及 client secret，原始 JSON 请求体不得改变。`:key` 必须等于 `plugin_key`，JSON 的 `model` 必须精确等于授权模型。仅调用已注册的原生任务插件提交路径，复用原生 TokenAuth、渠道分发和原生任务账本；不提供通用 Relay 或影策自行申报金额的结算接口。

内部 Token 即使经其他途径取得，也不能直接调用常规 Relay 或只读任务接口；只有本授权已领取且已验证原始请求后，才允许在指定原生插件提交路径使用一次。

服务端在模型调用前原子将 `authorized` 改为 `unknown`，第二次提交（包括超时、连接断开或进程重启）均返回 409，**绝不自动重发**。上游是否接受但尚未落库时保留 `unknown`，不可凭客户端超时退款或再授权同一个任务。成功写入原生任务与关联 `native_task_id` 在同一数据库事务内完成，变为 `submitted`；即使提交方断开，也尝试有界时间的持久化。预估金额超过授权上限则请求不进入上游；原生提交后和异步完成的最终金额限制在同一上限，超过部分不向用户扣取。失败任务的退款由原生任务结算决定，不能通过影策上报任意退款金额。

## 查询和不确定状态

`GET /api/internal/integrations/canvas/v1/relay/authorizations/:id?user_id=42`。仅返回相同 client 和真实用户的数据：`id`、`state`、`task_id`、`native_task_id`、`upstream_task_id`，已有原生任务时额外返回 `native_status` 和 `native_quota`。终态任务的 `state` 为 `success` 或 `failure`。`unknown` 没有原生任务关联时必须人工/对账调查，不自动重发或释放预扣；`rejected` 表示未进入上游调用；`authorized` 到期后也不可提交。接口不返回 Token Key、渠道密钥或原始任务正文。

内部 Token 的不可变用途标志同步进入 Redis 缓存；用户不能通过普通 Token 管理接口修改或删除该凭据。现有身份交换草稿与此接口共用机密，客户端服务端必须保证 `user_id` 来自已验证的本地账户绑定，不接受浏览器自行申报的数字 ID。

## 验证门槛

图片受限接入：new-api 提供 `GET /api/internal/integrations/canvas/v1/catalog`、`GET /catalog/offerings/:offeringId`（完整前缀同前者）和 `POST /catalog/refresh`。目录按当前用户可用分组、启用的渠道/模型筛选，offeringId 绑定 client、渠道、分组和模型；新出现或未进行健康检查的渠道状态为 `unknown`。最近 20 条检查记录与 7 天全部检查的可用率分开计算。渠道测试写入健康记录，不进行额外的用户额度生图探测。`refresh` 重新读取实时目录，不触发付费探测。

图片授权在原有授权请求中补 `capability=image`、`offering_id`，`plugin_key` 为 `image` 或 `image-edit`；`POST /api/internal/integrations/canvas/v1/relay/authorizations/:id/image/:key` 在单次领取、模型校验和渠道 pin 后复用原生 Images Relay 与原生账本。编辑仅对已实现 multipart Images 路径的 OpenAI 类型渠道公布。调用失败、超时或上游响应不确定时不得自动重发。管理员仍须确认目标模型名、实际价格/分组、错误响应和上游退款语义。

本接口仍需完成真实 SQLite、MySQL、PostgreSQL 的新库与升级迁移测试，以及上游接受后超时、进程崩溃、Redis/订阅资金来源和异步失败的端到端账本对账。未完成这些门槛前不得宣称可上线。
