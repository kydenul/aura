# grpc-gateway-demo

原生 grpc-go 服务，同进程内同时暴露：

- **gRPC 端口 `:5568`**：供内部服务间高性能调用
- **HTTP REST 端口 `:8080`**：供外部第三方程序 / 前端 / Postman 调用

核心方案：[grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway) —— 通过 proto 里的
`google.api.http` annotation 声明路由映射，自动生成一个反向代理层，把 HTTP/JSON 请求转换成 gRPC 调用。
**业务逻辑只写一份**（`internal/service/user.go`），HTTP 和 gRPC 两个入口共用。

```
proto/user.proto              proto 定义 + HTTP 路由映射（核心）
cmd/server/main.go            程序入口，同时起 gRPC + HTTP 两个 server
internal/service/user.go      业务逻辑实现（只写一份，两边复用）
internal/interceptor/
  ├── logging.go              gRPC 拦截器：请求日志
  ├── recovery.go             gRPC 拦截器：panic 兜底
  ├── auth.go                 gRPC 拦截器：Bearer Token 鉴权（HTTP/gRPC 共用同一套）
  └── http_middleware.go      HTTP middleware：CORS、日志
gen/proto/                    protoc 生成的代码（运行 generate.sh 后产生，不需要手写）
generate.sh                   一键生成 pb.go / grpc.pb.go / gw.pb.go
```

## 环境准备

```bash
# 1. protoc 编译器（Mac 示例，Linux 用 apt install protobuf-compiler）
brew install protobuf

# 2. Go 插件：生成消息类型 / gRPC stub / gateway 代码
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest

# 确保 $GOPATH/bin 在 PATH 里，否则 protoc 找不到上面装的插件
export PATH="$PATH:$(go env GOPATH)/bin"
```

## 运行步骤

```bash
# 1. 生成 pb 代码（首次运行会自动下载 google/api/*.proto 依赖）
./generate.sh

# 2. 拉取 Go 依赖
go mod tidy

# 3. 启动服务（同时监听 :5568 gRPC 和 :8080 HTTP）
go run ./cmd/server
```

看到这两行说明启动成功：

```
🚀 gRPC server listening on :5568
🚀 HTTP gateway listening on :8080
```

## 接口测试

### 创建用户（HTTP REST）

```bash
curl -X POST http://localhost:8080/v1/users \
  -H "Authorization: Bearer demo-token-123" \
  -H "Content-Type: application/json" \
  -d '{"name": "张三", "email": "zhangsan@example.com"}'
```

### 查询用户

```bash
curl http://localhost:8080/v1/users/u-1 \
  -H "Authorization: Bearer demo-token-123"
```

### 查询列表（分页）

```bash
curl "http://localhost:8080/v1/users?page=1&page_size=10" \
  -H "Authorization: Bearer demo-token-123"
```

### 更新用户

```bash
curl -X PATCH http://localhost:8080/v1/users/u-1 \
  -H "Authorization: Bearer demo-token-123" \
  -H "Content-Type: application/json" \
  -d '{"name": "李四"}'
```

### 删除用户

```bash
curl -X DELETE http://localhost:8080/v1/users/u-1 \
  -H "Authorization: Bearer demo-token-123"
```

### 不带 token（应返回 401 Unauthenticated）

```bash
curl http://localhost:8080/v1/users/u-1
```

### 用原生 gRPC 调用（用 grpcurl 工具）

```bash
grpcurl -plaintext \
  -H "authorization: Bearer demo-token-123" \
  -d '{"name": "王五", "email": "wangwu@example.com"}' \
  localhost:5568 user.v1.UserService/CreateUser
```

## 拦截器说明（你重点关心的部分）

| 拦截器                     | 作用域 | 文件                 | 说明                                                                                                                                                                      |
| -------------------------- | ------ | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `UnaryRecoveryInterceptor` | gRPC   | `recovery.go`        | 捕获业务代码 panic，转成标准错误返回，不拖垮进程                                                                                                                          |
| `UnaryLoggingInterceptor`  | gRPC   | `logging.go`         | 记录方法名、耗时、错误                                                                                                                                                    |
| `UnaryAuthInterceptor`     | gRPC   | `auth.go`            | Bearer Token 鉴权；**因为 HTTP 请求经 grpc-gateway 转发时 Authorization header 会自动透传成 gRPC metadata，所以这一份鉴权逻辑同时覆盖 HTTP 和 gRPC 两个入口，不用写两遍** |
| `CORSMiddleware`           | HTTP   | `http_middleware.go` | 标准 `net/http` middleware，包在 grpc-gateway 生成的 mux 外层                                                                                                             |
| `HTTPLoggingMiddleware`    | HTTP   | `http_middleware.go` | HTTP 入口日志，方便和 gRPC 日志对照排查                                                                                                                                   |

**关键点**：grpc-gateway 生成的 mux 本质就是一个标准 `http.Handler`，所以社区里任何现成的 HTTP middleware
（限流、gzip、JWT、Prometheus metrics 等，比如 [chi](https://github.com/go-chi/chi)、
[gorilla handlers](https://github.com/gorilla/handlers)）都能直接 `handler = someMiddleware(handler)` 包一层，
完全不需要为 gateway 专门写一套。

## 生产环境建议

- demo 用的是内存 map 存数据、明文 gRPC 连接，生产环境替换为真实 DB + 至少 TLS/mTLS
- `validateToken` 是演示用的写死校验，生产替换为 JWT 解析或调用统一鉴权服务
- 如果方法数量多、proto 文件多，建议把 `gen.sh` 升级成用 [`buf`](https://buf.build) 管理（配置文件
  `buf.yaml` + `buf.gen.yaml`），比裸 protoc 命令好维护
- 如果还需要自动生成 OpenAPI/Swagger 文档给前端，可以加上
  `protoc-gen-openapiv2` 插件，跟这套 gateway 是同一个生态、同一份 proto 驱动
