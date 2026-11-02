package cache

import (
	"context"
	"sync"
)

// flight 是一次进行中的生成：leader 写完 body / err 后 close(done)，等待者读同一份。
type flight struct {
	done chan struct{}
	body []byte
	err  error
}

// flightGroup 是 single-flight 的唯一实现：同一 id 的并发生成只做一次，其余等同一份结果。
//
// 两个实例：Cache（分片 / 关键帧索引）与 CoverCache（海报）。海报单独一组是因为
// 「同步本地化封面」与「请求 poster.jpg 按需回源」是同一个键上的同一件事——
// 它们共用一组才能让撞在一起的两个调用点只回源一次（MKACG-650）。
type flightGroup struct {
	mu      sync.Mutex // 只保护 flights；goroutine 之间不共享别的可变状态
	flights map[string]*flight
}

// newFlightGroup 构造一个空的 single-flight 组。
func newFlightGroup() *flightGroup {
	return &flightGroup{flights: make(map[string]*flight)}
}

// do 把同一 id 的并发生成合成一次：先到的是 leader，其余等它的结果。
//
// mutex 只保护 map（与 channel 分工，不混用）。leader 的 ctx 决定生成是否继续：
// 它被取消时等待者拿到同一个错误，各自的请求自己重试 —— 比在这里做生成的生命周期
// 管理便宜，也不会留下无人接收的结果。
//
// published 是「结果已发布、flight 尚未摘除」时的观察点，只给测试用：它在持锁状态下
// 调用，回调里只能做只读断言（或调不碰这把锁的 hit）。
func (g *flightGroup) do(ctx context.Context, id string, gen func() ([]byte, error), published func(id string)) ([]byte, error) {
	g.mu.Lock()
	if f, ok := g.flights[id]; ok {
		g.mu.Unlock()
		select {
		case <-f.done:
			return f.body, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	g.flights[id] = f
	g.mu.Unlock()

	// 先赋值再 close：close 建立 happens-before，等待者看不到半写的 body/err。
	f.body, f.err = gen()

	// 发布与摘除在同一把锁内，且发布在前：晚到者要么还能在 map 里看到这个 flight、
	// 等 done（结果已经可用，不重生成），要么看到它已消失 —— 而「已消失」意味着结果
	// 已发布，那时 hit() 必然命中。反过来（先摘除、解锁后再 close）会留出一个
	// 「map 里没有、结果又还没发布」的窗口，晚到者在窗口里当上第二个 leader，
	// 同一个分片被切两遍 —— MKACG-640 的 10 并发用例就是这么红的。
	g.mu.Lock()
	close(f.done)
	if published != nil {
		published(id)
	}
	delete(g.flights, id)
	g.mu.Unlock()
	return f.body, f.err
}
