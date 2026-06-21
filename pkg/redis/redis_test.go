package redis

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// ======================== 测试辅助 ========================

// setupClient 启动 miniredis 并返回封装后的 *Client。
// 同时返回 *miniredis.Miniredis，供测试直接操作 Redis 内部状态做断言。
func setupClient(t *testing.T) (*miniredis.Miniredis, *Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("启动 miniredis 失败: %v", err)
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = rdb.Close()
		mr.Close()
	})
	return mr, NewClientFromConn(rdb)
}

// ======================== NewClient / Init / 全局单例 ========================

func TestNewClient_ConnectsToMiniredis(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("启动 miniredis 失败: %v", err)
	}
	defer mr.Close()

	host, portStr, _ := net.SplitHostPort(mr.Addr())
	port, _ := strconv.Atoi(portStr)

	c, err := NewClient(Options{Host: host, Port: port})
	if err != nil {
		t.Fatalf("NewClient 应连接成功: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Set(context.Background(), "k", "v", 0); err != nil {
		t.Errorf("连接后 Set 应成功: %v", err)
	}
}

func TestNewClient_ConnectFailure(t *testing.T) {
	// 指向一个未监听的端口，并把超时/重试压到最小，避免拖慢测试。
	c, err := NewClient(Options{
		Host:        "127.0.0.1",
		Port:        1, // 基本不可能被 redis 占用
		DialTimeout: 100 * time.Millisecond,
		ReadTimeout: 100 * time.Millisecond,
		MaxRetries:  1,
	})
	if err == nil {
		_ = c.Close()
		t.Fatal("连接不可达地址应返回错误")
	}
	if c != nil {
		t.Errorf("失败时应返回 nil client，得 %v", c)
	}
}

func TestInit_SetsGlobalSingleton(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("启动 miniredis 失败: %v", err)
	}
	defer mr.Close()
	host, portStr, _ := net.SplitHostPort(mr.Addr())
	port, _ := strconv.Atoi(portStr)

	t.Cleanup(func() { globalClient = nil })

	if IsInitialized() {
		t.Fatal("初始化前 IsInitialized 应为 false")
	}
	if err := Init(Options{Host: host, Port: port}); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !IsInitialized() {
		t.Error("Init 后 IsInitialized 应为 true")
	}
	if Get() == nil {
		t.Error("Init 后 Get() 不应为 nil")
	}
}

func TestNewClientFromConn_NotNil(t *testing.T) {
	_, c := setupClient(t)
	if c == nil {
		t.Fatal("NewClientFromConn 不应返回 nil")
	}
}

// ======================== 通用 KV 操作 ========================

func TestSet_Get_Del(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()

	if err := c.Set(ctx, "k1", "v1", 0); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	val, err := c.Get(ctx, "k1")
	if err != nil || val != "v1" {
		t.Errorf("Get 失败: val=%q err=%v", val, err)
	}
	if err := c.Del(ctx, "k1"); err != nil {
		t.Fatalf("Del 失败: %v", err)
	}
	if _, err := c.Get(ctx, "k1"); !errors.Is(err, Nil) {
		t.Errorf("Del 后 Get 应返回 Nil，得 %v", err)
	}
}

func TestDel_MultipleKeys(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "d1", "1", 0)
	_ = c.Set(ctx, "d2", "2", 0)

	if err := c.Del(ctx, "d1", "d2"); err != nil {
		t.Fatalf("多 key Del 失败: %v", err)
	}
	n, _ := c.Exists(ctx, "d1", "d2")
	if n != 0 {
		t.Errorf("Del 后应都不存在，得 n=%d", n)
	}
}

func TestDel_EmptyKeys_NoError(t *testing.T) {
	_, c := setupClient(t)
	if err := c.Del(context.Background()); err != nil {
		t.Errorf("空 keys Del 不应报错，得 %v", err)
	}
}

func TestExists(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()

	_ = c.Set(ctx, "e1", "1", 0)
	n, err := c.Exists(ctx, "e1", "e2")
	if err != nil || n != 1 {
		t.Errorf("Exists 失败: n=%d err=%v", n, err)
	}
}

func TestExists_EmptyKeys(t *testing.T) {
	_, c := setupClient(t)
	n, err := c.Exists(context.Background())
	if err != nil || n != 0 {
		t.Errorf("空 keys Exists 应为 0，得 n=%d err=%v", n, err)
	}
}

func TestMGet_HitsAndMisses(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "k1", "v1", 0)
	_ = c.Set(ctx, "k3", "v3", 0)

	results, err := c.MGet(ctx, "k1", "k2", "k3")
	if err != nil {
		t.Fatalf("MGet 失败: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("结果长度应为 3，得 %d", len(results))
	}
	if results[0] != "v1" || results[1] != nil || results[2] != "v3" {
		t.Errorf("MGet 结果不符: %v", results)
	}
}

func TestMGet_EmptyKeys(t *testing.T) {
	_, c := setupClient(t)
	results, err := c.MGet(context.Background())
	if err != nil || len(results) != 0 {
		t.Errorf("空 keys MGet 应返回空，得 results=%v err=%v", results, err)
	}
}

func TestExpire_TTL(t *testing.T) {
	mr, c := setupClient(t)
	ctx := context.Background()

	_ = c.Set(ctx, "ttl_key", "val", 0)
	ok, err := c.Expire(ctx, "ttl_key", 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("Expire 失败: ok=%v err=%v", ok, err)
	}
	d, err := c.TTL(ctx, "ttl_key")
	if err != nil || d <= 0 {
		t.Errorf("TTL 应大于 0，d=%v err=%v", d, err)
	}

	mr.FastForward(11 * time.Second)
	if _, err := c.Get(ctx, "ttl_key"); !errors.Is(err, Nil) {
		t.Errorf("过期后 key 应消失，err=%v", err)
	}
}

func TestSet_WithExpiration(t *testing.T) {
	mr, c := setupClient(t)
	ctx := context.Background()

	_ = c.Set(ctx, "exp_key", "v", 5*time.Second)
	mr.FastForward(6 * time.Second)
	if _, err := c.Get(ctx, "exp_key"); !errors.Is(err, Nil) {
		t.Errorf("带 TTL 的 key 过期后应消失，err=%v", err)
	}
}

func TestIncr_Decr_IncrBy(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()

	if n, _ := c.Incr(ctx, "counter"); n != 1 {
		t.Errorf("第 1 次 Incr 应为 1，得 %d", n)
	}
	if n, _ := c.Incr(ctx, "counter"); n != 2 {
		t.Errorf("第 2 次 Incr 应为 2，得 %d", n)
	}
	if n, _ := c.Decr(ctx, "counter"); n != 1 {
		t.Errorf("Decr 后应为 1，得 %d", n)
	}
	if n, err := c.IncrBy(ctx, "counter", 5); err != nil || n != 6 {
		t.Errorf("IncrBy 失败: n=%d err=%v", n, err)
	}
}

// ======================== 原子操作 ========================

func TestSetNX(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()

	ok, err := c.SetNX(ctx, "nx_key", "first", 0)
	if err != nil || !ok {
		t.Fatalf("第 1 次 SetNX 应成功: ok=%v err=%v", ok, err)
	}
	ok, err = c.SetNX(ctx, "nx_key", "second", 0)
	if err != nil || ok {
		t.Errorf("第 2 次 SetNX 应失败: ok=%v err=%v", ok, err)
	}
	if val, _ := c.Get(ctx, "nx_key"); val != "first" {
		t.Errorf("SetNX 不应覆盖已有值，得 %q", val)
	}
}

func TestGetDel(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()

	_ = c.Set(ctx, "gd", "hello", 0)
	val, err := c.GetDel(ctx, "gd")
	if err != nil || val != "hello" {
		t.Errorf("GetDel 失败: val=%q err=%v", val, err)
	}
	if _, err := c.Get(ctx, "gd"); !errors.Is(err, Nil) {
		t.Errorf("GetDel 后 key 应消失，err=%v", err)
	}
}

// ======================== Hash ========================

func TestHash_HSet_HGet_HDel_HGetAll(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()
	key := "hash_test"

	if err := c.HSet(ctx, key, "f1", "v1"); err != nil {
		t.Fatalf("HSet 失败: %v", err)
	}
	_ = c.HSet(ctx, key, "f2", "v2")

	if val, err := c.HGet(ctx, key, "f1"); err != nil || val != "v1" {
		t.Errorf("HGet 失败: val=%q err=%v", val, err)
	}
	if all, err := c.HGetAll(ctx, key); err != nil || len(all) != 2 {
		t.Errorf("HGetAll 失败: len=%d err=%v", len(all), err)
	}
	if err := c.HDel(ctx, key, "f1"); err != nil {
		t.Fatalf("HDel 失败: %v", err)
	}
	if _, err := c.HGet(ctx, key, "f1"); !errors.Is(err, Nil) {
		t.Errorf("HDel 后 HGet 应返回 Nil，得 %v", err)
	}
}

func TestHGet_MissingField_ReturnsNil(t *testing.T) {
	_, c := setupClient(t)
	if _, err := c.HGet(context.Background(), "no_hash", "no_field"); !errors.Is(err, Nil) {
		t.Errorf("不存在的 Hash 字段应返回 Nil，得 %v", err)
	}
}

// ======================== Set（集合） ========================

func TestSet_SAdd_SRem_SMembers_SIsMember(t *testing.T) {
	_, c := setupClient(t)
	ctx := context.Background()
	key := "set_test"

	n, err := c.SAdd(ctx, key, "a", "b", "c")
	if err != nil || n != 3 {
		t.Fatalf("SAdd 失败: n=%d err=%v", n, err)
	}
	if ok, _ := c.SIsMember(ctx, key, "b"); !ok {
		t.Error("SIsMember('b') 应为 true")
	}
	if ok, _ := c.SIsMember(ctx, key, "z"); ok {
		t.Error("SIsMember('z') 应为 false")
	}
	if members, err := c.SMembers(ctx, key); err != nil || len(members) != 3 {
		t.Errorf("SMembers 失败: len=%d err=%v", len(members), err)
	}
	if removed, err := c.SRem(ctx, key, "a"); err != nil || removed != 1 {
		t.Errorf("SRem 失败: removed=%d err=%v", removed, err)
	}
}

// ======================== ZSet（有序集合） ========================

func TestZSet_ZRemRangeByScore_ZCard_ZRem(t *testing.T) {
	mr, c := setupClient(t)
	ctx := context.Background()
	key := "zset_test"

	// 直接用底层 client 写入带分数的成员（go-redis v9 用值类型 redis.Z）。
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	now := float64(time.Now().Unix())
	rdb.ZAdd(
		ctx, key,
		goredis.Z{Score: now - 100, Member: "expired1"},
		goredis.Z{Score: now - 50, Member: "expired2"},
		goredis.Z{Score: now + 100, Member: "active"},
	)

	if card, err := c.ZCard(ctx, key); err != nil || card != 3 {
		t.Fatalf("ZCard 初始应为 3，得 %d err=%v", card, err)
	}
	removed, err := c.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(int64(now), 10))
	if err != nil || removed != 2 {
		t.Errorf("ZRemRangeByScore 应删除 2 个，得 removed=%d err=%v", removed, err)
	}
	if card, _ := c.ZCard(ctx, key); card != 1 {
		t.Errorf("删除后 ZCard 应为 1，得 %d", card)
	}
	if err := c.ZRem(ctx, key, "active"); err != nil {
		t.Errorf("ZRem 失败: %v", err)
	}
	if card, _ := c.ZCard(ctx, key); card != 0 {
		t.Errorf("ZRem 后 ZCard 应为 0，得 %d", card)
	}
}

// ======================== Pub/Sub ========================

func TestPublish_PSubscribe(t *testing.T) {
	mr, c := setupClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pubsub := c.PSubscribe(ctx, "chan:*")
	defer func() { _ = pubsub.Close() }()

	if err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("PSubscribe 确认失败: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		mr.Publish("chan:hello", "world")
	}()

	select {
	case msg := <-pubsub.Channel():
		if msg.Payload != "world" || msg.Channel != "chan:hello" {
			t.Errorf("消息不符: channel=%q payload=%q", msg.Channel, msg.Payload)
		}
	case <-ctx.Done():
		t.Error("超时未收到 Pub/Sub 消息")
	}
}

func TestPublish_NoSubscriber_NoError(t *testing.T) {
	_, c := setupClient(t)
	if err := c.Publish(context.Background(), "no_sub_chan", "msg"); err != nil {
		t.Errorf("无订阅者时 Publish 不应报错，err=%v", err)
	}
}

// ======================== Nil 哨兵 / Close ========================

func TestNilSentinel_IsGoredisNil(t *testing.T) {
	if !errors.Is(Nil, goredis.Nil) {
		t.Error("Nil 应等价于 goredis.Nil")
	}
}

func TestClose_NoError(t *testing.T) {
	_, c := setupClient(t)
	if err := c.Close(); err != nil {
		t.Errorf("Close 不应报错，err=%v", err)
	}
}
