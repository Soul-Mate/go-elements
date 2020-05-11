package main

import "sync"

func main() {
	var m sync.Map
	println("Store k1")
	m.Store("k1", "v1") // dirty[k1]
	m.Load("k1")        // miss=1 enough, dirty -> read [k1], dirty = nil

	println("Store k2")
	m.Store("k2", "v2") // k2 -> dirty
	m.Load("k2")        // miss = 2, read[k1], dirty[k2]

	m.Delete("k1")

	println("Store k3")
	m.Store("k3", "v3") // k3 -> dirty
	m.Load("k3")        // miss = 3, read[k1], dirty[k2, k3]

	println("Store k4")
	m.Store("k4", "v4") // k4 -> dirty
	m.Load("k4")        // miss = 4, read[k1], dirty[k2,k3,k4]
	m.Load("k4")        // miss = 4 enough dirty -> read [k1, k2, k3, k4]
	m.Load("k4")

	m.Delete("k1")
	m.Delete("k2")
	m.Delete("k3")
	m.Delete("k4")
	m.Load("k1")

}
