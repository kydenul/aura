#!/usr/bin/env bash

# 生成 pb.go / grpc.pb.go / gw.pb.go 三件套
# 依赖：protoc, protoc-gen-go, protoc-gen-go-grpc, protoc-gen-grpc-gateway
set -euo pipefail

cd "$(dirname "$0")"

# 拉取 grpc-gateway 编译所需的标准依赖 proto（只需首次执行）
THIRD_PARTY_DIR="./../third_party/google/api"
if [ ! -f "$THIRD_PARTY_DIR/annotations.proto" ]; then
  echo "📥 下载 google/api/annotations.proto, http.proto ..."
  mkdir -p "$THIRD_PARTY_DIR"
  curl -sL https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/annotations.proto \
    -o "$THIRD_PARTY_DIR/annotations.proto"
  curl -sL https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/http.proto \
    -o "$THIRD_PARTY_DIR/http.proto"
fi

mkdir -p ./../gen/proto

protoc \
  -I ./../proto \
  -I ./../third_party \
  --go_out=./../gen/proto --go_opt=paths=source_relative \
  --go-grpc_out=./../gen/proto --go-grpc_opt=paths=source_relative \
  --grpc-gateway_out=./../gen/proto --grpc-gateway_opt=paths=source_relative \
  user.proto

echo "✅ 代码生成完成，输出目录：./gen/proto"
echo "   - user.pb.go      (消息类型)"
echo "   - user_grpc.pb.go (gRPC client/server stub)"
echo "   - user.pb.gw.go   (HTTP <-> gRPC 网关代码)"
