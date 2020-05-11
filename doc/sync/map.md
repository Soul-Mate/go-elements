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


**1. double-check**

double-check技术是lock free编程中常用的手法, 更详细的内容可以参考 [Wiki: Double-checked locking
](https://en.wikipedia.org/wiki/Double-checked_locking)

在Load这段代码中, double-check是必须的, 原因是并发执行下, dirty 会被提升为 read (发生在而在 `!ok&&read.amended` 代码之间), 从而在dirty中访问不到, 但read中访问到。

**2. `m.missLocked()`**

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
![](https://upload-images.jianshu.io/upload_images/14252596-0dc5e75e33a7707a.png?imageMogr2/auto-orient/strip%7CimageView2/2/w/1240)

现在执行如下操作
| 操作     | misses | read.m | misses < len(dirty) |
| -------- | ------ | :----- | ------------------- |
| 初始状态 | 0      | nil    | true                |
| Load k1  | 1      | nil    | true                |
| Load k2  | 2      | nil    | true                |
| Load k3  | 3      | nil    | true                | 

接着 Load k4, 此时 misses == len(dirty), 因此 dirty会 上升为 read
![](https://upload-images.jianshu.io/upload_images/14252596-c8fe0eb59d8648ff.png?imageMogr2/auto-orient/strip%7CimageView2/2/w/1240)
那么很容易想到，接下来如果继续访问上面的key，都会在read.m中命中

**load 值**
确定了entry后, 调用e.load() 返回真正的 value
```go
// 实现的atomic.Value Load, 对应entry
func (e *entry) load() (value interface{}, ok bool) {
	// 从atomic.Value中加载出对应的指针
	p := atomic.LoadPointer(&e.p)
	
	// 这里会在执行一次检查, 为什么呢？
	// 因为在Load()方法中, 仅仅是确定了 key对应的entry在哪里,
	// 如果entry在dirty中, 则会通过这个检查, 但如果在read.m中的entry,
	// 根据sync.Map的设计, entry可能处于nil或expunged的状态 (表示不存在或标记为删除)
	if p == nil || p == expunged {
		return nil, false
	}
	return *(*interface{})(p), true
}
```

### 并发Load的情况

| 操作           | t1               | t2               | misses < len(dirty)        | 获取数据的map |
| -------------- | ---------------- | ---------------- | -------------------------- | ------------- |
| t1, t2 Load k1 | 执行, lock       | 等待unlock       | 1, true                    | dirty         |
|                | 执行结束, unlock | 执行, lock       | 2, true                    | dirty         |
|                |                  | 执行结束, unlock |                            |               |
| t2, t2 Load k2 | 等待unlock       | 执行, lock       | 3, false, dirty 上升为read | dirty         |
|                | 执行, lock       | 执行结束, unlock |                            | read.m        |
|                | 执行结束, unlock |                  |                            |               |
1. t1 和 t2 同时并发 Load k1, t1检查完read.m后, t1抢占到Mutex,  从dirty中获取，miss + 1 < len (dirty)
2. t2检查完read.m后，等待t1 unlock
3. t1 unlock后, t2继续执行, 从dirty中获取, miss + 1 < len(dirty)
4. t1 和 t2 同时并发 Load k2, 这次t2抢占到Mutex, 从dirty中获取, **miss + 1 == len(dirty)**, 因此m.dirty提升为read.m
5. **在t2导致dirty提升read.m之前 , t1 检查完read.m后**, 并没有在read.m后检索到，此时t1等待t2 unlock
6. t2 unlock后, t1继续执行, 这时进入 **double-check, t1在read.m中检测到了刚刚由t2触发的提升的dirty中的数据**, t1 unlock, 返回。

或者下图可以更清晰的表示这个过程

![某个线程即将触发dirty提升操作时并发Load](https://upload-images.jianshu.io/upload_images/14252596-17aea077e0345d88.png?imageMogr2/auto-orient/strip%7CimageView2/2/w/1240)

## Store操作

Store的具体实现如下
```go
// Store sets the value for a key.
func (m *Map) Store(key, value interface{}) {
	read, _ := m.read.Load().(readOnly)
	// key存在于read.m, tryStore尝试存储新的value,
	// tryStore成功直接返回
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}

	// tryStroe失败, lock住开始继续操作
	m.mu.Lock()

	read, _ = m.read.Load().(readOnly)

	if e, ok := read.m[key]; ok {
		// read.m中有对应的entry, 但被设置为expunged,
		// 因此不可再read.m中使用了, 这里将entry设置为unexpunge并
		// 存储到dirty
		if e.unexpungeLocked() {
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			m.dirty[key] = e
		}

		e.storeLocked(&value)
	} else if e, ok := m.dirty[key]; ok {
		// read.m中没找到, dirty中找到, 更新dirty中对应的value
		e.storeLocked(&value)
	} else {
		// !read.amended 表示dirty为nil,
		// 需要创建dirty并复制read.m到新的dirty
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.

			// 从read复制到dirty中
			m.dirtyLocked()

			// 将read.amended 标记为 true
			m.read.Store(readOnly{m: read.m, amended: true})
		}

		// read.amended表示dirty不为nil, 直接将新的
		// kv存储到dirty中
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
}
```
Store的基本思路是：
- read.m[key] ok，则通过tryStore进行update，update成功后直接返回
- tryStore失败，key对应的value是expunged状态，进行double-check后，如果read.m[key] ok，则将read.m[key]得到的entry设置为unexpunged状态存储到dirty中
- 如果read.m[key] !ok,  但m.dirty[key] 是ok的，则直接update m.dirty[key]后返回
- 如果m.dirty[key] 也不ok, 如果!read.amended, 表示m.dirty为nil, 需要创建m.dirty, 同时将read.m中不是nil和状态不为expunged的entry复制到m.dirty
- 最后m.dirty[key] store对应的value

### tryStore 的实现
```go
func (e *entry) tryStore(i *interface{}) bool {
	for {
		p := atomic.LoadPointer(&e.p)
		// read.m中的entry状态为expunged, 不会去Store新的值
		if p == expunged {
			return false
		}

		// 使用CAS操作存储新的值
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}
```
当entry不是expunged的时候，tryStore使用CAS更新value

### dirtyLocked的实现
```go
func (m *Map) dirtyLocked() {
	// 仅在dirty存在时才会进行拷贝
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	// 从read复制到dirty
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		// e不是nil或unexpunged的状态下, 才会复制到dirty
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}
```
## Delete实现

Delete 的代码如下

```go
func (m *Map) Delete(key interface{}) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		// double-check
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]

		if !ok && read.amended {
			// 从dirty删除
			delete(m.dirty, key)
		}
		m.mu.Unlock()
	}
	// 从read.m中删除
	if ok {
		e.delete()
	}
}
```
Delete的基本思路
- read.m[key]找到则删除
- read.m[key]没有找到并且read.amended (dirty存在), double-check后如果read.m[key]找到了, 则unlock后删除
- 否则从m.dirty中删除

### delete实现
```go
func (e *entry) delete() (hadValue bool) {
	for {
		p := atomic.LoadPointer(&e.p)
		// p已经是删除状态
		if p == nil || p == expunged {
			return false
		}
		// 使用CAS设置p=nil
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return true
		}
	}
}
```
实际的value清除由delete实现，如果value是nil或者expunged，则不进行CAS

## Range实现
```go
func (m *Map) Range(f func(key, value interface{}) bool) {
	read, _ := m.read.Load().(readOnly)

	// 只要read.amended为true, 则dirty中存在数据且数据没有提升到read
	if read.amended {
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		// double-check
		if read.amended {
			// 拷贝m.dirty
			read = readOnly{m: m.dirty}
			m.read.Store(read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	// 遍历并传入到user func
	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}
```
因为是thread-safe的map，因此提供了一个Range方法来提供map的遍历，Range实现思路是：
如果read.amended = true, 说明有数据存在dirty不存在read, 此时需要lock copy, 之后遍历read就是lock free的了

##未完待续...