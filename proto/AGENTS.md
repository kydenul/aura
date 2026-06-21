# proto · L2

Protobuf 协议定义。**改接口的源头**：HTTP 路由与 gRPC 方法都由这里一份 proto 驱动，改完必须重新生成代码。

## 目录

| 文件                           | 说明                                                                           |
| ------------------------------ | ------------------------------------------------------------------------------ |
| `user.proto`                   | 协议源文件（手写）：`UserService` 服务 + 消息类型 + `google.api.http` 路由映射 |
| `../gen/proto/user.pb.go`      | `protoc-gen-go` 生成的消息类型（**勿手改**）                                   |
| `../gen/proto/user_grpc.pb.go` | `protoc-gen-go-grpc` 生成的 server / client stub（**勿手改**）                 |
| `../gen/proto/user.pb.gw.go`   | `protoc-gen-grpc-gateway` 生成的 HTTP↔gRPC 网关代码（**勿手改**）              |

## HTTP ↔ gRPC 映射

每个 RPC 用 `option (google.api.http)` 声明对应的 REST 路由，gateway 据此生成反向代理：

| RPC          | HTTP                                     | 说明                                       |
| ------------ | ---------------------------------------- | ------------------------------------------ |
| `CreateUser` | `POST /v1/users`（`body: "*"`）          | 整个 body 映射为请求                       |
| `GetUser`    | `GET /v1/users/{id}`                     | 路径参数 `{id}` 映射到 `GetUserRequest.id` |
| `ListUsers`  | `GET /v1/users`                          | query 参数 `?page=&page_size=` 映射到字段  |
| `UpdateUser` | `PATCH /v1/users/{id}`（`body: "user"`） | body 映射到 `UpdateUserRequest.user`       |
| `DeleteUser` | `DELETE /v1/users/{id}`                  | 返回 `google.protobuf.Empty`               |

> 字段定义直接读 `user.proto`（含注释），不在此重复。

## 重新生成

修改 `user.proto` 后必须重新生成代码：

```bash
make proto        # 等价于 ./scripts/generate.sh
```

首次执行会自动下载 `google/api/annotations.proto`、`http.proto` 到 `third_party/`（grpc-gateway 编译依赖）。生成产物覆盖 `gen/proto/`，**不要手改**。

## 约束

- **新增字段**：用新的 tag number，不要复用历史 tag。
- **删除字段**：禁止；改为 `reserved` 防止 tag 重用。
- **改类型**：禁止；用新字段。
- **改 HTTP 路由**：只改 `option (google.api.http)`，重新生成即可，无需动 Go 代码。

## 常见入口

| 想做什么        | 改哪里                                                                                                                        |
| --------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| 加一个 RPC      | `user.proto` 加 `rpc` + `google.api.http` → `make proto` → 在 `internal/service` 实现方法                                     |
| 改请求/响应字段 | `user.proto` 改 message → `make proto` → 调整 `internal/service` 实现                                                         |
| 只改 REST 路径  | 改 `option (google.api.http)` → `make proto`（Go 逻辑不变）                                                                   |
| 新增一个服务    | `proto/` 加新 `.proto`（或在 `user.proto` 加 service）→ `make proto` → 在 `internal/service` 实现 + `cmd/server/main.go` 注册 |
