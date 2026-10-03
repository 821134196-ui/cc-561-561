# 外卖商户异常订单申诉与证据归档系统

门店收到超时、缺品、拒单等处罚后，员工整理证据材料，店长在申诉期限内正式提交，平台结论回传后可查看处理结果；全过程证据版本留痕。

## 一条命令启动

```bash
./run.sh          # 等价于 go run ./cmd/server，首次会自动下载依赖
```

打开 http://localhost:8080 。演示账号（密码均为 `123456`）：

| 门店 | 店长 | 员工 |
|---|---|---|
| 鲜丰水果（文一店） | `manager1` | `staff1` |
| 鲜丰水果（文二店） | `manager2` | `staff2` |

运行测试：`go test ./...`（覆盖跨门店隔离、期限边界、重复回执幂等、证据版本保留）。

## 技术栈与目录

- 后端：Go + Gin + GORM，SQLite（纯 Go 驱动 `glebarez/sqlite`，无需 CGO），数据文件 `data/app.db`
- 前端：Vue 3（本地 vendor，无构建步骤），由 Gin 直接托管 `web/`
- 附件：本地磁盘 `uploads/<处罚ID>/`
- 平台接口与消息接口：本地模拟（见下）

```
cmd/server/        入口（配置、迁移、种子、HTTP 服务）
internal/app/      模型、认证、申诉/证据/回执业务、模拟平台、种子、测试
web/               Vue 前端（index.html / app.js / style.css / vendor/vue）
run.sh             一键启动
```

## 业务规则实现

- **每条处罚单独计算期限**：`penalties.deadline = issued_at + 72h`，提交校验 `now > deadline` 拒绝并明确提示“已超过申诉期限（截止 …）”；边界含端点（恰在截止时刻可提交，见 `TestDeadlineBoundary`）。
- **同一订单多条处罚**：订单与处罚一对多，列表/详情按处罚维度展示各自的期限与状态。
- **订单时间线与证据缺口**：详情页展示原订单时间线（接单/出餐/打包/取餐/送达），并按处罚类型要求比对出餐、打包、骑手取餐三类关键证据，缺项明确列出（`GET /api/penalties/:id` 的 `missing` 字段）。
- **证据不可替换**：证据文件只有创建接口，没有更新/删除接口；提交后草稿上传被禁止，补件、撤回、平台更正都会生成新的 `EvidenceVersion`（含材料快照），历史版本原样保留。
- **回执幂等**：`platform_receipts.receipt_id` 唯一。相同回执重复送达 → 返回已有结论（`duplicate: true`）；相同 receipt_id 但内容不一致 → `409`，保留原结论；已出结论的申诉收到新回执 → 拒绝。绝不产生两个相互冲突的结果（`TestDuplicateReceiptIdempotent`）。
- **结论关联材料**：`appeal_results.version_no` 记录结论所依据的材料版本，详情页展示回执号、结论（维持/撤销/部分调整+金额）与关联版本。
- **数据隔离与角色**：所有查询按 `store_id` 过滤，跨门店访问一律 404（处罚、申诉、证据文件均覆盖）；员工可上传/整理证据，正式提交、补件、撤回仅店长可执行（`403` 并提示）。
- **消息接口**：处罚下达、申诉提交、补件、撤回、平台结论、平台更正均写入门店通知（`GET /api/notifications`）。

## 本地模拟的平台接口

| 接口 | 说明 |
|---|---|
| `POST /api/platform/receipts` | 平台结论回执（请求头 `X-Platform-Token: local-sim`），幂等处理 |
| `POST /api/platform/corrections` | 平台更正，生成 correction 版本 |
| `POST /api/dev/appeals/:id/simulate-result` | 立即生成一次平台结论（提交后默认 3 秒也会自动给出） |
| `POST /api/dev/appeals/:id/redeliver-receipt` | 重复送达上次回执，验证幂等 |

详情页底部“模拟平台（本地）”面板可直接触发上述操作。

## 主要 API

```
POST /api/login                        登录（Bearer Token）
GET  /api/penalties                    本门店处罚列表（含倒计时）
GET  /api/penalties/:id                详情：时间线/证据缺口/材料/申诉
POST /api/penalties/:id/evidence       上传证据（员工+店长，提交前）
POST /api/penalties/:id/submit         正式提交申诉（仅店长，期限内）
POST /api/appeals/:id/supplement       补件（仅店长，生成新版本）
POST /api/appeals/:id/withdraw         撤回（仅店长，留痕）
GET  /api/appeals/:id                  申诉版本与结论
GET  /api/files/:id                    证据文件下载（门店隔离）
GET  /api/notifications                消息列表
```

环境变量：`ADDR`（默认 `:8080`）、`DB_PATH`、`UPLOAD_DIR`、`WEB_DIR`。
