// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package workload

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// LoadEvent represent a key access that generates load against the database.
// TODO(kvoli): The single key interface is expensive when parsed. Consider
// pre-aggregating load events into batches to amortize this cost.
type LoadEvent struct {
	Key        int64
	Writes     int64
	WriteSize  int64
	Reads      int64
	ReadSize   int64
	RequestCPU int64
	RaftCPU    int64
}

// LoadBatch is a sorted list of load events.
type LoadBatch []LoadEvent

// Less is part of the sort interface.
func (lb LoadBatch) Less(i, j int) bool {
	return lb[i].Key < lb[j].Key
}

// Swap is part of the sort interface.
func (lb LoadBatch) Swap(i, j int) {
	lb[i], lb[j] = lb[j], lb[i]
}

// Len is part of the sort interface.
func (lb LoadBatch) Len() int {
	return len(lb)
}

// Generator generates a workload where each op contains: key,
// op type (e.g., read/write), size.
type Generator interface {
	// Tick returns the load events up till time tick, from the last time the
	// workload generator was called.
	Tick(tick time.Time) LoadBatch
}

// RandomGenerator generates random operations within some limits.
type RandomGenerator struct {
	seed                int64
	keyGenerator        KeyGenerator
	rand                *rand.Rand
	lastRun             time.Time
	rollsPerSecond      float64
	readRatio           float64
	maxSize             int
	minSize             int
	requestCPUPerAccess int64
	raftCPUPerWrite     int64
}

// NewRandomGenerator returns a generator that generates random operations
// within some limits.
func NewRandomGenerator(
	start time.Time,
	seed int64,
	keyGenerator KeyGenerator,
	rate float64,
	readRatio float64,
	maxSize int,
	minSize int,
	requestCPUPerAccess int64,
	raftCPUPerWrite int64,
) Generator {
	return newRandomGenerator(start, seed, keyGenerator, rate, readRatio, maxSize, minSize, requestCPUPerAccess, raftCPUPerWrite)
}

// newRandomGenerator returns a generator that generates random operations
// within some limits.
func newRandomGenerator(
	start time.Time,
	seed int64,
	keyGenerator KeyGenerator,
	rate float64,
	readRatio float64,
	maxSize int,
	minSize int,
	requestCPUPerAccess int64,
	raftCPUPerWrite int64,
) *RandomGenerator {
	return &RandomGenerator{
		seed:                seed,
		keyGenerator:        keyGenerator,
		rand:                keyGenerator.rand(),
		lastRun:             start,
		rollsPerSecond:      rate,
		readRatio:           readRatio,
		maxSize:             maxSize,
		minSize:             minSize,
		requestCPUPerAccess: requestCPUPerAccess,
		raftCPUPerWrite:     raftCPUPerWrite,
	}
}

// Tick returns the load events up till time tick, from the last time the
// workload generator was called.
func (rwg *RandomGenerator) Tick(maxTime time.Time) LoadBatch {
	elapsed := maxTime.Sub(rwg.lastRun).Seconds()
	count := int(elapsed * rwg.rollsPerSecond)
	// Do not attempt to generate additional load events if the elapsed
	// duration is not sufficiently large. If we did, this would bump the last
	// run to maxTime and we may end up in a cycle where no events are ever
	// generated if the rate of load events is less than the interval at which
	// this function is called.
	if count < 1 {
		return LoadBatch{}
	}
	// TODO(kvoli): In profiling, this map constitutes the majority of the run
	// time when sampling (40%). We should investigate using an array that
	// never decreases in size, where an index represents a key. In practice,
	// this would avoid the need for hashing and dynamic allocation. Assuming
	// the key span is small, this would produce better result. We could revert
	// to using a map when the rate/keyspan is low and the distribution is
	// sparse (e.g. zipfian distribution).
	next := make(map[int64]LoadEvent)

	// Here we skew slightly towards writes to take the difference in rounding.
	reads := int(float64(count) * rwg.readRatio)
	writes := count - reads

	// We aggregate write and reads that occur on the same key. This reduces
	// the number of distinct load events when there is a high collision rate.
	for read := 0; read < reads; read++ {
		size := int64(rwg.rand.Intn(rwg.maxSize-rwg.minSize+1) + rwg.minSize)
		key := rwg.keyGenerator.readKey()
		event := next[key]
		event.Reads++
		event.ReadSize += size
		event.RequestCPU += rwg.requestCPUPerAccess
		next[key] = event
	}

	for write := 0; write < writes; write++ {
		size := int64(rwg.rand.Intn(rwg.maxSize-rwg.minSize+1) + rwg.minSize)
		key := rwg.keyGenerator.writeKey()
		event := next[key]
		event.Writes++
		event.WriteSize += size
		event.RequestCPU += rwg.requestCPUPerAccess
		event.RaftCPU += rwg.raftCPUPerWrite
		next[key] = event
	}

	ret := make(LoadBatch, len(next))
	i := 0
	for k, v := range next {
		v.Key = k
		ret[i] = v
		i++
	}

	sort.Sort(ret)
	rwg.lastRun = maxTime
	return ret
}

// TODO(wenyihu6): Instead of duplicating the key generator logic in simulators,
// we should directly reuse the code from the repo pkg/workload/(kv|ycsb) to
// ensure consistent testing.

// KeyGenerator generates read and write keys.
type KeyGenerator interface {
	writeKey() int64
	readKey() int64
	rand() *rand.Rand
}

// uniformGenerator generates keys with a uniform distribution. Note that keys
// do not necessarily need to be written before they may have a read issued
// against them.
type uniformGenerator struct {
	min, max int64
	random   *rand.Rand
}

// NewUniformKeyGen returns a key generator that generates keys with a
// uniform distribution.
func NewUniformKeyGen(min, max int64, rand *rand.Rand) KeyGenerator {
	if max <= min {
		panic(fmt.Sprintf("max (%d) must be greater than min (%d)", max, min))
	}
	return &uniformGenerator{
		min:    min,
		max:    max,
		random: rand,
	}
}

func (g *uniformGenerator) writeKey() int64 {
	return g.random.Int63n(g.max-g.min) + g.min
}

func (g *uniformGenerator) readKey() int64 {
	return g.random.Int63n(g.max-g.min) + g.min
}

func (g *uniformGenerator) rand() *rand.Rand {
	return g.random
}

// zipfianGenerator generates keys with a power-rank distribution. Note that keys
// do not necessarily need to be written before they may have a read issued
// against them.
type zipfianGenerator struct {
	min, max int64
	random   *rand.Rand
	zipf     *rand.Zipf
}

// NewZipfianKeyGen returns a key generator that generates reads and writes
// following a Zipfian distribution. Where few keys are relatively frequent,
// whilst the others are infrequently accessed. Cycle is defined as max-min.
// The generator generates values k ∈ [0, cycle] such that P(k) is proportional
// to (v + k) ** (-s).
// Requirements: cycle > 0, s > 1, and v >= 1
func NewZipfianKeyGen(min, max int64, s float64, v float64, random *rand.Rand) KeyGenerator {
	if max <= min {
		panic(fmt.Sprintf("max (%d) must be greater than min (%d)", max, min))
	}
	return &zipfianGenerator{
		min:    min,
		max:    max,
		random: random,
		zipf:   rand.NewZipf(random, s, v, uint64(max-min)),
	}
}

func (g *zipfianGenerator) writeKey() int64 {
	return int64(g.zipf.Uint64()) + g.min
}

func (g *zipfianGenerator) readKey() int64 {
	return int64(g.zipf.Uint64()) + g.min
}

func (g *zipfianGenerator) rand() *rand.Rand {
	return g.random
}

// TestCreateWorkloadGenerator creates a simple uniform workload generator that
// will generate load events at the rate given. The read ratio is fixed to
// 0.95.
func TestCreateWorkloadGenerator(seed int64, start time.Time, rate int, keySpan int64) Generator {
	readRatio := 0.95
	minWriteSize := 128
	maxWriteSize := 256
	workloadRate := float64(rate)
	r := rand.New(rand.NewSource(seed))

	return NewRandomGenerator(
		start,
		seed,
		NewUniformKeyGen(0, keySpan, r),
		workloadRate,
		readRatio,
		maxWriteSize,
		minWriteSize,
		0, /* requestCPUPerAccess */
		0, /* raftCPUPerWrite */
	)
}
