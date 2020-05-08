## 介绍

sync.Map是go官方实现的一个thread safe的map, 适合读多写少的场景


## 数据结构

```go
type Map struct {
	mu Mutex // 用于read map无法命中的情况下, 回去操作dirty map, 此时会加锁
    read atomic.Value // read map, 特点是这个map的操作是CAS lock free的
    
    // dirty map 初始为 nil, 当 read map 在操作无法命中的时候, 
    // 回去操作 dirty map, 特点是要 lock
	dirty map[interface{}]*entry 
    
     // 每当访问map时, read map 无法命中, 
     // 会递增 misses, misses = len(dirty) 时, 会进行提升操作
    misses int
}

// read 中的 atomic.Value 实际保存的是 readOnly
type readOnly struct {
	m       map[interface{}]*entry // 存放 readonly map, 初始时为nil;
    
	// amended 初始时为false, read map 和 dirty map 都为nil, 
	// 运行过程中会不断改变状态:
    // 1. dirty提升为 read map 的时候, amended 会设置为 false, 表示 dirty 当前没有数据, 当 read.amended == false 时: 
	//  	Load 查找不到k时, 不会从dirty找
	//  	Stroe 写入不存在的kv时, 将read中的元素拷贝到dirty, 并设置 read.amended = true
	//  	Delete 删除不存在的k时, 不会从dirty删除
	// 
	// 2. dirty对read map进行copy后, 会将amended设置为 true
    amended bool                   
}

var expunged = unsafe.Pointer(new(interface{}))

type entry struct {
	p unsafe.Pointer // *interface{}
}
```


## Load操作

Load的具体实现如下

```go
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	// !ok说明read.m中没有, 如果read.amended == true,
	// 说明存在于dirty中, lock住从dirty中查找
	if !ok && read.amended {
		m.mu.Lock()

		// double-check, 原因在于!ok && read.amended不是原子的, 并发运行
		// 过程中, 另一个线程的访问可能会将dirty提升为read.m, 提升后数据会在read.m中, 同时
		// read.amended会设置为false, 因此需要double-check一次, 如果read.m中有数据直接返回
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]

		if !ok && read.amended {
			e, ok = m.dirty[key]
		
			// 计算miss次数, 如果达到miss上限则提升read为dirty
			m.missLocked()
		}
		m.mu.Unlock()
	}

	// here, 说明没有数据
	if !ok {
		return nil, false
	}

	return e.load()
}
```

Load的函数实现还是非常简洁和清晰的, 里面都两个需要注意的地方
1. double-check
2. `m.missLocked()`


**double-check**

double-check技术是lock free中常用的手法, 更详细的内容可以参考 [Wiki: Double-checked locking
](https://en.wikipedia.org/wiki/Double-checked_locking)

在Load这段代码中, double-check是必须的, 原因是并发执行下, dirty 会被提升为 read (发生在而在 `!ok&&read.amended` 代码之间), 从而在dirty中访问不到, 但read中访问到。

**`m.missLocked()`**

`m.missLocked()` 方法算是sync.Map高效执行的原因之一, 该调用做了如下的工作: 

```go
// locked during execution
func (m *Map) missLocked() {
	// 递增 misses
	m.misses++

	// 当misses次数小于len(m.dirty)时, 不做任何工作
	if m.misses < len(m.dirty) {
		return
	}

	// 当misses次数大于len(m.dirty)时, 提升dirty map为read map,
	// 同时隐式的amended是false
	m.read.Store(readOnly{m: m.dirty})

	// dirty设置为nil
	m.dirty = nil
	// miss计数设置为0
	m.misses = 0
}
```

例如现在read.m和m.dirty情况如下所示


k1 -> v1,
k2 -> v2,
k3 -> v3,
k4 -> v4

k1 Store v1的时候, 此时dirty = nil, read = nil, read.amended = false, 表明数据没有在read (显然的)

程序执行 m.dirtyLocked(), 由于 m.dirty = nil, len(read.m) == 0, 因此会创建出一个bucket为0的m.dirty, 并不会进行实际的从read -> dirty

k1 Load v1的时候, 此时read.amended = true, 表明数据在dirty不在read. 回去dirty查找, 然后misses + 1, 此时 misses = len(m.dirty), 因此dirty -> read, 并且read.amended = false, dirty = 0, dirty.misses = 0

k2 Store v2, 此时read.amended = false, dirty = nil, len(read) = 1, 因此会创建一个长度为1 m.dirty, 同时将k1 -> dirty, 并存储k2, 将read.amended标识为true.
 
现在read.m存储了 [k1], dirty.m存储了[k1, k2]

k2 Load v2, read.amended = true, 则会进入missLocked, misses++ < len(m.dirty), 因此k2还是会在dirty中, 不会提升.


当misses一定数量的时候, dirty -> read, dirty 会为nil, read.amended = false, 表示数据在read中. 此时写入一个新key, read.amended = false 表示数据在read中, 需要执行dirtyLocked , 从read -> dirty拷贝一份, 设置为read.amended = true, 表示数据在dirty中, 但对旧的key(在read中的key)不会影响Load和Store, 对于新key的写入, 由于 read.amended=true表示dirty此时没有提升, 因此不需要执行dirtyLocked.

为什么要从read copy 到 dirty呢？


```go
func (m *Map) dirtyLocked() {
	// 此时m.dirty == nil
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	// read.m == 0, 此时仅仅会创建m.dirty, 不会进行read copy to dirty
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		// e不是nil或unexpunged的状态下, 才会复制到dirty
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}
```