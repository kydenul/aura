package hmac

import (
	"context"
	"time"
)

// NonceStore 防重放 nonce 去重存储。方案文档约定：在 [ts, ts+window] 内 (appid,nonce) 全局唯一。
//
// 本包不绑定具体存储；中间件通常注入一个基于 Redis SETNX 的实现
// （SETNX hmac_nonce_<appid>_<nonce> 1 EX <ttl>）。为 nil 时中间件跳过去重（仅靠时间窗防重放）。
//
// TTL 选择：Verify 的时间窗判定是开区间（|now-ts|>Skew 才拒），存储侧过期通常是「整秒
// 闭区间」，两者叠加会出现「ts 仍在窗口、nonce 已过期」的边界缝隙——同一签名可在此缝隙
// 内被重放。调用方应至少传入 2×Skew 作为 TTL，覆盖正负向最大可接受偏移并留足时钟漂移
// 与过期精度余量。
type NonceStore interface {
	// FirstSeen 原子登记 (appID,nonce)：首次出现返回 true 并占位 ttl；已存在返回 false（重放）。
	FirstSeen(ctx context.Context, appID, nonce string, ttl time.Duration) (bool, error)
}
