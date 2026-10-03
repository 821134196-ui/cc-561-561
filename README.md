# 外卖商户异常订单申诉与证据归档系统

门店收到 **超时 / 缺品 / 拒单** 处罚后：员工整理出餐、打包、骑手取餐等证据，
**店长在申诉期限内正式提交**，随后可查看平台的维持处罚 / 撤销 / 部分调整结论。

- 后端：Go + Gin + GORM + SQLite（WAL），附件落本地磁盘
- 前端：Vue 3（本地 vendor 运行时，**无需 npm/构建步骤**），由 Go `embed` 托管，单二进制部署
- 平台回执、消息通知均为**本地模拟**，页面内置「模拟平台」面板
- 一条命令启动：`./run.sh`，浏览器打开 http://localhost:8080

## 演示账号（密码均为 `123456`）

| 账号 | 门店 | 角色 | 权限 |
|---|---|---|---|
| `wang` | 望京一店 (A001) | 店长 | 整理证据 + 正式提交 / 补件 / 撤回 + 模拟平台回执 |
| `zhang` | 望京一店 (A001) | 员工 | 只能整理、删除草稿证据 |
| `li` | 国贸二店 (B002) | 店长 | B 店数据，与 A 店完全隔离 |
| `chen` | 国贸二店 (B002) | 员工 | B 店员工 |

种子数据覆盖：待申诉（含临近到期）、已超期、审核中、已撤回、已结案，
且同一订单上挂有**两条期限独立计算**的处罚。

## 启动

```bash
./run.sh                 # 默认 :8080，数据在 ./data（SQLite + uploads）
ADDR=:9090 ./run.sh      # 自定义端口
rm -rf ./data            # 想恢复初始演示数据时删除数据目录即可
```

后端测试：`go test ./...`（含 `-race` 并发用例）。

## 核心业务规则与实现位置

1. **订单时间线 + 缺证提示**：打开处罚详情即展示下单/接单/出餐/打包/骑手取餐/送达时间线；
   按处罚类型要求出餐（超时）、打包（缺品）等槽位，缺失槽位在页面与提交接口双重校验
   （`internal/appeal/service.go: MissingSlots / Submit`，HTTP 422）。
2. **同一订单多条处罚，期限各自独立**：每条处罚有独立 `deadline_at`，
   详情页展示同订单的其他处罚及其独立倒计时。
3. **证据只增不改（版本链）**：提交前为可删的草稿；提交动作把草稿一次性归档（`appeal_actions.snapshot_ids`
   保存本次材料快照）。提交后补件立即生成 `supplement` 动作 + 新版本（`supersedes_id` 指向旧版），
   旧文件与元数据永久保留；归档版本删除/替换一律 409。
4. **提交 / 撤回 / 平台更正均留痕**：动作链 `submit → supplement → withdraw → submit → result → correction`
   只追加；平台更正（新 `receipt_key` + `kind=correction`）更新当前生效结论，
   旧回执通过 `supersedes_receipt_id` 完整保留。
5. **角色**：员工可上传/删除草稿；`submit / withdraw / supplement / receipts` 接口要求店长（403）。
6. **跨店隔离**：所有查询都带 `store_id`，路径门店与会话门店不一致一律 **404**（不暴露资源存在性），
   附件下载同样校验归属；附件目录不开放无鉴权静态访问。
7. **期限边界**：以 `!now.After(deadline)` 判定——截止时刻当刻仍可提交，过 1 纳秒即超期（409），
   超期提交时同时把状态落库为 `expired` 并发通知；后台每分钟 sweep 一次，页面倒计时到点自动变超期样式。
8. **回执幂等（重点）**：`platform_receipts.receipt_key` 全局唯一索引兜底；
   同一回执重复送达只 `delivery_count++`，**绝不产生第二条结论、第二条动作或第二条通知**，
   即使重复报文内容与原结论冲突（如先 upheld 再送 revoked）也不会改写结论。
   事务使用 `BEGIN IMMEDIATE` + 唯一冲突回查 + 应用层重试，
   8 并发重复推送同一回执的竞态由测试固定（见 `TestDuplicateReceipt`）。
9. **消息模拟**：平台结论/超期生成门店内通知（顶栏铃铛，未读角标，同 ref 只通知一次）。

## API 摘要（均在 `/api/stores/:storeId/...` 下，Bearer token 鉴权）

```
POST   /api/auth/login
GET    /api/stores/:sid/dashboard
GET    /api/stores/:sid/penalties?status=
GET    /api/stores/:sid/penalties/:id
POST   /api/stores/:sid/penalties/:id/draft          multipart，员工可用
DELETE /api/stores/:sid/penalties/:id/evidence/:vid  仅未归档草稿
POST   /api/stores/:sid/penalties/:id/submit         店长；超期409/缺证422
POST   /api/stores/:sid/penalties/:id/withdraw       店长
POST   /api/stores/:sid/penalties/:id/supplement     店长，审核中补件
POST   /api/stores/:sid/penalties/:id/receipts       店长，模拟平台回执（幂等）
GET    /api/evidence/:vid/download                   带门店归属校验
GET/POST /api/stores/:sid/notifications(/read)
```
