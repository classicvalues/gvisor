// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package metric provides primitives for collecting metrics.
package metric

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"gvisor.dev/gvisor/pkg/eventchannel"
	"gvisor.dev/gvisor/pkg/log"
	pb "gvisor.dev/gvisor/pkg/metric/metric_go_proto"
	"gvisor.dev/gvisor/pkg/sync"
)

var (
	// ErrNameInUse indicates that another metric is already defined for
	// the given name.
	ErrNameInUse = errors.New("metric name already in use")

	// ErrInitializationDone indicates that the caller tried to create a
	// new metric after initialization.
	ErrInitializationDone = errors.New("metric cannot be created after initialization is complete")

	// ErrFieldValueContainsIllegalChar indicates that the value of a metric
	// field had an invalid character in it.
	ErrFieldValueContainsIllegalChar = errors.New("metric field value contains illegal character")

	// WeirdnessMetric is a metric with fields created to track the number
	// of weird occurrences such as time fallback, partial_result, vsyscall
	// count, watchdog startup timeouts and stuck tasks.
	WeirdnessMetric = MustCreateNewUint64Metric("/weirdness", true /* sync */, "Increment for weird occurrences of problems such as time fallback, partial result, vsyscalls invoked in the sandbox, watchdog startup timeouts and stuck tasks.",
		Field{
			name:          "weirdness_type",
			allowedValues: []string{"time_fallback", "partial_result", "vsyscall_count", "watchdog_stuck_startup", "watchdog_stuck_tasks"},
		})

	// SuspiciousOperationsMetric is a metric with fields created to detect
	// operations such as opening an executable file to write from a gofer.
	SuspiciousOperationsMetric = MustCreateNewUint64Metric("/suspicious_operations", true /* sync */, "Increment for suspicious operations such as opening an executable file to write from a gofer.",
		Field{
			name:          "operation_type",
			allowedValues: []string{"opened_write_execute_file"},
		})
)

// InitStage is the name of a Sentry initialization stage.
type InitStage string

// List of all Sentry initialization stages.
var (
	InitRestoreConfig InitStage = "restore_config"
	InitExecConfig    InitStage = "exec_config"
	InitRestore       InitStage = "restore"
	InitCreateProcess InitStage = "create_process"
	InitTaskStart     InitStage = "task_start"

	// allStages is the list of allowed stages.
	allStages = []InitStage{
		InitRestoreConfig,
		InitExecConfig,
		InitRestore,
		InitCreateProcess,
		InitTaskStart,
	}
)

// Uint64Metric encapsulates a uint64 that represents some kind of metric to be
// monitored. We currently support metrics with at most one field.
//
// Metrics are not saved across save/restore and thus reset to zero on restore.
//
// TODO(b/67298427): Support metric fields.
type Uint64Metric struct {
	// value is the actual value of the metric. It must be accessed atomically.
	value uint64

	// numFields is the number of metric fields. It is immutable once
	// initialized.
	numFields int

	// mu protects the below fields.
	mu sync.RWMutex `state:"nosave"`

	// fields is the map of fields in the metric.
	fields map[string]uint64
}

var (
	// initialized indicates that all metrics are registered. allMetrics is
	// immutable once initialized is true.
	initialized bool

	// allMetrics are the registered metrics.
	allMetrics = makeMetricSet()
)

// Initialize sends a metric registration event over the event channel.
//
// Precondition:
//  * All metrics are registered.
//  * Initialize/Disable has not been called.
func Initialize() error {
	if initialized {
		return errors.New("metric.Initialize called after metric.Initialize or metric.Disable")
	}

	m := pb.MetricRegistration{}
	for _, v := range allMetrics.uint64Metrics {
		m.Metrics = append(m.Metrics, v.metadata)
	}
	for _, v := range allMetrics.distributionMetrics {
		m.Metrics = append(m.Metrics, v.metadata)
	}
	m.Stages = make([]string, 0, len(allStages))
	for _, s := range allStages {
		m.Stages = append(m.Stages, string(s))
	}
	if err := eventchannel.Emit(&m); err != nil {
		return fmt.Errorf("unable to emit metric initialize event: %w", err)
	}

	initialized = true
	return nil
}

// Disable sends an empty metric registration event over the event channel,
// disabling metric collection.
//
// Precondition:
//  * All metrics are registered.
//  * Initialize/Disable has not been called.
func Disable() error {
	if initialized {
		return errors.New("metric.Disable called after metric.Initialize or metric.Disable")
	}

	m := pb.MetricRegistration{}
	if err := eventchannel.Emit(&m); err != nil {
		return fmt.Errorf("unable to emit metric disable event: %w", err)
	}

	initialized = true
	return nil
}

type customUint64Metric struct {
	// metadata describes the metric. It is immutable.
	metadata *pb.MetricMetadata

	// value returns the current value of the metric for the given set of
	// fields. It takes a variadic number of field values as argument.
	value func(fieldValues ...string) uint64
}

// Field contains the field name and allowed values for the metric which is
// used in registration of the metric.
type Field struct {
	// name is the metric field name.
	name string

	// allowedValues is the list of allowed values for the field.
	allowedValues []string
}

// NewField defines a new Field that can be used to break down a metric.
func NewField(name string, allowedValues []string) Field {
	return Field{
		name:          name,
		allowedValues: allowedValues,
	}
}

// toProto returns the proto definition of this field, for use in metric
// metadata.
func (f Field) toProto() *pb.MetricMetadata_Field {
	return &pb.MetricMetadata_Field{
		FieldName:     f.name,
		AllowedValues: f.allowedValues,
	}
}

// multiFieldToKey returns a concatenated version of the given fields.
// It can be used as a unique key within multi-dimensional metrics.
// Does not allow commas as valid character within field values.
func multiFieldToKey(fields ...string) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	for _, f := range fields {
		if strings.ContainsRune(f, ',') {
			return "", ErrFieldValueContainsIllegalChar
		}
	}
	return strings.Join(fields, ","), nil
}

// keyToMultiField is the reverse of multiFieldToKey.
func keyToMultiField(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, ",")
}

// fieldMapper provides multi-dimensional fields to a single concatenated key
// that can be used as string key for multi-dimensional metrics.
// fieldMapper is a recursive struct, but its lookup function is not.
// It pays for its allocation-free, low-stack lookup by preallocating a map of
// all possible field values, so it is memory-hungry.
type fieldMapper struct {
	// depth is 0 at the lowest level of fieldMapper.
	depth int
	// key is set only at the lowest level of fieldMapper, i.e. depth == 0.
	// It contains the full concatenated key of all the parent field values.
	key string
	// children is set only at depth > 0.
	// For depth=d, children[fields[d]] is the fieldMapper that can be used to
	// look up keys for fields[d+1:].
	children map[string]fieldMapper
}

// newFieldMapper returns a new fieldMapper for the given set of fields.
func newFieldMapper(fields ...Field) (fieldMapper, error) {
	var initFieldMapper func(values []string, remaining ...Field) (fieldMapper, error)
	initFieldMapper = func(values []string, remaining ...Field) (fieldMapper, error) {
		depth := len(remaining)
		if depth == 0 {
			key, err := multiFieldToKey(values...)
			if err != nil {
				return fieldMapper{}, err
			}
			return fieldMapper{key: key}, nil
		}
		current := remaining[0]
		children := make(map[string]fieldMapper, len(current.allowedValues))
		for _, value := range current.allowedValues {
			newValues := make([]string, len(values)+1)
			copy(newValues, values)
			newValues[len(values)] = value
			child, err := initFieldMapper(newValues, remaining[1:]...)
			if err != nil {
				return fieldMapper{}, err
			}
			children[value] = child
		}
		return fieldMapper{
			depth:    depth,
			children: children,
		}, nil
	}
	return initFieldMapper(nil, fields...)
}

// lookup looks up a key within the fieldMapper.
// It needs to allocate no memory and be nosplit-compatible, so it cannot be
// recursive.
// This *must* be called with the correct number of fields, or it will panic.
// +checkescape:all
//go:nosplit
func (m fieldMapper) lookup(fields ...string) string {
	depth := len(fields)
	if depth != m.depth {
		panic("invalid field lookup depth")
	}
	var found bool
	for i := 0; i < depth; i++ {
		if m, found = m.children[fields[i]]; !found {
			panic("disallowed field value")
		}
	}
	return m.key
}

// lookupConcat looks up a key within the fieldMapper where the fields are
// the concatenation of two list of fields.
// It needs to allocate no memory and be nosplit-compatible, so it cannot be
// recursive, and cannot allocate a concatenated []string.
// This *must* be called with the correct number of fields, or it will panic.
// +checkescape:all
//go:nosplit
func (m fieldMapper) lookupConcat(fields1, fields2 []string) string {
	depth1 := len(fields1)
	depth2 := len(fields2)
	if depth1+depth2 != m.depth {
		panic("invalid field lookup depth")
	}
	var found bool
	for i := 0; i < depth1; i++ {
		if m, found = m.children[fields1[i]]; !found {
			panic("disallowed field value")
		}
	}
	for i := 0; i < depth2; i++ {
		if m, found = m.children[fields2[i]]; !found {
			panic("disallowed field value")
		}
	}
	return m.key
}

// all iterates over all keys within the fieldMapper.
func (m fieldMapper) all() []string {
	var all []string
	var visit func(fm fieldMapper)
	visit = func(fm fieldMapper) {
		if fm.depth == 0 {
			all = append(all, fm.key)
		} else {
			for _, child := range fm.children {
				visit(child)
			}
		}
	}
	visit(m)
	return all
}

// RegisterCustomUint64Metric registers a metric with the given name.
//
// Register must only be called at init and will return and error if called
// after Initialized.
//
// Preconditions:
// * name must be globally unique.
// * Initialize/Disable have not been called.
// * value is expected to accept exactly len(fields) arguments.
func RegisterCustomUint64Metric(name string, cumulative, sync bool, units pb.MetricMetadata_Units, description string, value func(...string) uint64, fields ...Field) error {
	if initialized {
		return ErrInitializationDone
	}

	if _, ok := allMetrics.uint64Metrics[name]; ok {
		return ErrNameInUse
	}
	if _, ok := allMetrics.distributionMetrics[name]; ok {
		return ErrNameInUse
	}

	allMetrics.uint64Metrics[name] = customUint64Metric{
		metadata: &pb.MetricMetadata{
			Name:        name,
			Description: description,
			Cumulative:  cumulative,
			Sync:        sync,
			Type:        pb.MetricMetadata_TYPE_UINT64,
			Units:       units,
		},
		value: value,
	}

	// Metrics can exist without fields.
	if l := len(fields); l > 1 {
		return fmt.Errorf("%d fields provided, must be <= 1", l)
	}

	for _, field := range fields {
		allMetrics.uint64Metrics[name].metadata.Fields = append(allMetrics.uint64Metrics[name].metadata.Fields, field.toProto())
	}
	return nil
}

// MustRegisterCustomUint64Metric calls RegisterCustomUint64Metric for metrics
// without fields and panics if it returns an error.
func MustRegisterCustomUint64Metric(name string, cumulative, sync bool, description string, value func(...string) uint64, fields ...Field) {
	if err := RegisterCustomUint64Metric(name, cumulative, sync, pb.MetricMetadata_UNITS_NONE, description, value, fields...); err != nil {
		panic(fmt.Sprintf("Unable to register metric %q: %s", name, err))
	}
}

// NewUint64Metric creates and registers a new cumulative metric with the given
// name.
//
// Metrics must be statically defined (i.e., at init).
func NewUint64Metric(name string, sync bool, units pb.MetricMetadata_Units, description string, fields ...Field) (*Uint64Metric, error) {
	m := Uint64Metric{
		numFields: len(fields),
	}

	if m.numFields == 1 {
		m.fields = make(map[string]uint64)
		for _, fieldValue := range fields[0].allowedValues {
			m.fields[fieldValue] = 0
		}
	}
	return &m, RegisterCustomUint64Metric(name, true /* cumulative */, sync, units, description, m.Value, fields...)
}

// MustCreateNewUint64Metric calls NewUint64Metric and panics if it returns an
// error.
func MustCreateNewUint64Metric(name string, sync bool, description string, fields ...Field) *Uint64Metric {
	m, err := NewUint64Metric(name, sync, pb.MetricMetadata_UNITS_NONE, description, fields...)
	if err != nil {
		panic(fmt.Sprintf("Unable to create metric %q: %s", name, err))
	}
	return m
}

// MustCreateNewUint64NanosecondsMetric calls NewUint64Metric and panics if it
// returns an error.
func MustCreateNewUint64NanosecondsMetric(name string, sync bool, description string) *Uint64Metric {
	m, err := NewUint64Metric(name, sync, pb.MetricMetadata_UNITS_NANOSECONDS, description)
	if err != nil {
		panic(fmt.Sprintf("Unable to create metric %q: %s", name, err))
	}
	return m
}

// Value returns the current value of the metric for the given set of fields.
func (m *Uint64Metric) Value(fieldValues ...string) uint64 {
	if m.numFields != len(fieldValues) {
		panic(fmt.Sprintf("Number of fieldValues %d is not equal to the number of metric fields %d", len(fieldValues), m.numFields))
	}

	switch m.numFields {
	case 0:
		return atomic.LoadUint64(&m.value)
	case 1:
		m.mu.RLock()
		defer m.mu.RUnlock()

		fieldValue := fieldValues[0]
		if _, ok := m.fields[fieldValue]; !ok {
			panic(fmt.Sprintf("Metric does not allow to have field value %s", fieldValue))
		}
		return m.fields[fieldValue]
	default:
		panic("Sentry metrics do not support more than one field")
	}
}

// Increment increments the metric field by 1.
func (m *Uint64Metric) Increment(fieldValues ...string) {
	m.IncrementBy(1, fieldValues...)
}

// IncrementBy increments the metric by v.
func (m *Uint64Metric) IncrementBy(v uint64, fieldValues ...string) {
	if m.numFields != len(fieldValues) {
		panic(fmt.Sprintf("Number of fieldValues %d is not equal to the number of metric fields %d", len(fieldValues), m.numFields))
	}

	switch m.numFields {
	case 0:
		atomic.AddUint64(&m.value, v)
		return
	case 1:
		fieldValue := fieldValues[0]
		m.mu.Lock()
		defer m.mu.Unlock()

		if _, ok := m.fields[fieldValue]; !ok {
			panic(fmt.Sprintf("Metric does not allow to have field value %s", fieldValue))
		}
		m.fields[fieldValue] += v
	default:
		panic("Sentry metrics do not support more than one field")
	}
}

// Bucketer is an interface to bucket values into finite, distinct buckets.
type Bucketer interface {
	// NumFiniteBuckets is the number of finite buckets in the distribution.
	// This is only called once and never expected to return a different value.
	NumFiniteBuckets() int

	// LowerBound takes the index of a bucket (within [0, NumBuckets()]) and
	// returns the inclusive lower bound of that bucket.
	// In other words, the lowest value of `x` for which `BucketIndex(x) == i`
	// should be `x = LowerBound(i)`.
	// The upper bound of a bucket is the lower bound of the next bucket.
	// The last bucket (with `bucketIndex == NumFiniteBuckets()`) is infinite,
	// i.e. it has no upper bound (but it still has a lower bound).
	LowerBound(bucketIndex int) int64

	// BucketIndex takes a sample and returns the index of the bucket that the
	// sample should fall into.
	// Must return either:
	//   - A value within [0, NumBuckets() -1] if the sample falls within a
	//     finite bucket
	//   - NumBuckets() if the sample falls within the last (infinite) bucket
	//   - '-1' if the sample is lower than what any bucket can represent, i.e.
	//     the sample should be in the implicit "underflow" bucket.
	// This function must be go:nosplit-compatible and have no escapes.
	// +checkescape:all
	BucketIndex(sample int64) int
}

// ExponentialBucketer implements Bucketer, with the first bucket starting
// with 0 as lowest bound with `Width` width, and each subsequent bucket being
// wider by a scaled exponentially-growing series, until `NumFiniteBuckets`
// buckets exist.
type ExponentialBucketer struct {
	// numFinitebuckets is the total number of finite buckets in the scheme.
	numFiniteBuckets int

	// width is the size of the first (0-th) finite bucket.
	width float64

	// scale is a factor applied uniformly to the exponential growth portion
	// of the bucket size.
	scale float64

	// growth is the exponential growth factor for finite buckets.
	// The n-th bucket is `growth` times wider than the (n-1)-th bucket.
	// Bucket sizes are floored, so `width` and `growth` must be large enough
	// such that the second bucket is actually wider than the first after
	// flooring (unless, of course, fixed-width buckets are what's desired).
	growth float64

	// growthLog is math.Log(growth).
	growthLog float64

	// maxSample is the max sample value which can be represented in a finite
	// bucket.
	maxSample int64

	// lowerbounds is a precomputed set of lower bounds of the buckets.
	// The "underflow" bucket has no lower bound, so it is not included here.
	// lowerBounds[0] is the lower bound of the first finite bucket, which is
	// also the upper bound of the underflow bucket.
	// lowerBounds[numFiniteBuckets] is the lower bound of the overflow bucket.
	lowerBounds []int64
}

// Minimum/maximum finite buckets for exponential bucketers.
const (
	exponentialMinBuckets = 1
	exponentialMaxBuckets = 100
)

// NewExponentialBucketer returns a new Bucketer with exponential buckets.
func NewExponentialBucketer(numFiniteBuckets int, width uint64, scale, growth float64) *ExponentialBucketer {
	if numFiniteBuckets < exponentialMinBuckets || numFiniteBuckets > exponentialMaxBuckets {
		panic(fmt.Sprintf("number of finite buckets must be in [%d, %d]", exponentialMinBuckets, exponentialMaxBuckets))
	}
	b := &ExponentialBucketer{
		numFiniteBuckets: numFiniteBuckets,
		width:            float64(width),
		scale:            scale,
		growth:           growth,
		growthLog:        math.Log(growth),
		lowerBounds:      make([]int64, numFiniteBuckets+1),
	}
	b.lowerBounds[0] = 0
	for i := 1; i <= numFiniteBuckets; i++ {
		b.lowerBounds[i] = int64(b.width*float64(i) + b.scale*math.Pow(b.growth, float64(i-1)))
	}
	b.maxSample = b.lowerBounds[numFiniteBuckets] - 1
	return b
}

// NumFiniteBuckets implements Bucketer.NumFiniteBuckets.
func (b *ExponentialBucketer) NumFiniteBuckets() int {
	return int(b.numFiniteBuckets)
}

// LowerBound implements Bucketer.LowerBound.
func (b *ExponentialBucketer) LowerBound(bucketIndex int) int64 {
	return b.lowerBounds[bucketIndex]
}

// BucketIndex implements Bucketer.BucketIndex.
// +checkescape:all
//go:nosplit
func (b *ExponentialBucketer) BucketIndex(sample int64) int {
	if sample < 0 {
		return -1
	}
	if sample == 0 {
		return 0
	}
	if sample > b.maxSample {
		return b.numFiniteBuckets
	}
	// Do a binary search. For the number of buckets we expect to deal with in
	// this code (a few dozen at most), this may be faster than computing a
	// logarithm. We can't use recursion because this would violate go:nosplit.
	lowIndex := 0
	highIndex := b.numFiniteBuckets
	for {
		pivotIndex := (highIndex + lowIndex) >> 1
		lowerBound := b.lowerBounds[pivotIndex]
		if sample < lowerBound {
			highIndex = pivotIndex
			continue
		}
		upperBound := b.lowerBounds[pivotIndex+1]
		if sample >= upperBound {
			lowIndex = pivotIndex
			continue
		}
		return pivotIndex
	}
}

// Verify that ExponentialBucketer implements Bucketer.
var _ = (Bucketer)((*ExponentialBucketer)(nil))

// DistributionMetric represents a distribution of values in finite buckets.
// It also separately keeps track of min/max in order to ascertain whether the
// buckets can faithfully represent the range of values encountered in the
// distribution.
type DistributionMetric struct {
	// exponentialBucketer is the bucketing scheme used for this metric.
	// Because we need DistributionMetric.AddSample to be go:nosplit-compatible,
	// we cannot use an interface reference here, as we would not be able to call
	// it in AddSample. Instead, we need one field per Bucketer implementation,
	// and we call whichever one is in use in AddSample.
	exponentialBucketer *ExponentialBucketer

	// metadata is the metadata about this metric.
	metadata *pb.MetricMetadata

	// fieldsToKey converts a multi-dimensional fields to a single string to use
	// as key for `samples`.
	fieldsToKey fieldMapper

	// samples is the number of samples that fell within each bucket.
	// It is mapped by the concatenation of the fields, using fieldsToKey.
	// The value is a list of bucket sample counts, with the 0-th being the
	// "underflow bucket", i.e. the bucket of samples which cannot fall into
	// any bucket that the bucketer supports.
	// The i-th value is the number of samples that fell into the bucketer's
	// (i-1)-th finite bucket.
	// The last value is the number of samples that fell into the bucketer's
	// last (i.e. infinite) bucket.
	samples map[string][]uint64
}

// NewDistributionMetric creates and registers a new distribution metric.
func NewDistributionMetric(name string, sync bool, bucketer Bucketer, unit pb.MetricMetadata_Units, description string, fields ...Field) (*DistributionMetric, error) {
	if initialized {
		return nil, ErrInitializationDone
	}
	if _, ok := allMetrics.uint64Metrics[name]; ok {
		return nil, ErrNameInUse
	}
	if _, ok := allMetrics.distributionMetrics[name]; ok {
		return nil, ErrNameInUse
	}

	var exponentialBucketer *ExponentialBucketer
	if expBucketer, ok := bucketer.(*ExponentialBucketer); ok {
		exponentialBucketer = expBucketer
	} else {
		return nil, fmt.Errorf("unsupported bucketer implementation: %T", bucketer)
	}
	fieldsToKey, err := newFieldMapper(fields...)
	if err != nil {
		return nil, err
	}
	allKeys := fieldsToKey.all()
	samples := make(map[string][]uint64, len(allKeys))
	numFiniteBuckets := bucketer.NumFiniteBuckets()
	for _, key := range allKeys {
		samples[key] = make([]uint64, numFiniteBuckets+2)
	}
	protoFields := make([]*pb.MetricMetadata_Field, len(fields))
	for i, f := range fields {
		protoFields[i] = f.toProto()
	}
	lowerBounds := make([]int64, numFiniteBuckets+1)
	for i := 0; i <= numFiniteBuckets; i++ {
		lowerBounds[i] = bucketer.LowerBound(i)
	}
	allMetrics.distributionMetrics[name] = &DistributionMetric{
		exponentialBucketer: exponentialBucketer,
		fieldsToKey:         fieldsToKey,
		samples:             samples,
		metadata: &pb.MetricMetadata{
			Name:                          name,
			Description:                   description,
			Cumulative:                    false,
			Sync:                          sync,
			Type:                          pb.MetricMetadata_TYPE_DISTRIBUTION,
			Units:                         unit,
			Fields:                        protoFields,
			DistributionBucketLowerBounds: lowerBounds,
		},
	}
	return allMetrics.distributionMetrics[name], nil
}

// MustRegisterDistributionMetric creates and registers a distribution metric.
// If an error occurs, it panics.
func MustRegisterDistributionMetric(name string, sync bool, bucketer Bucketer, unit pb.MetricMetadata_Units, description string, fields ...Field) *DistributionMetric {
	distrib, err := NewDistributionMetric(name, sync, bucketer, unit, description, fields...)
	if err != nil {
		panic(err)
	}
	return distrib
}

// AddSample adds a sample to the distribution.
// This *must* be called with the correct number of fields, or it will panic.
// +checkescape:all
//go:nosplit
func (d *DistributionMetric) AddSample(sample int64, fields ...string) {
	d.addSampleByKey(sample, d.fieldsToKey.lookup(fields...))
}

// addSampleByKey works like AddSample, with the field key already known.
// +checkescape:all
//go:nosplit
func (d *DistributionMetric) addSampleByKey(sample int64, key string) {
	bucket := d.exponentialBucketer.BucketIndex(sample)
	atomic.AddUint64(&d.samples[key][bucket+1], 1)
}

// Minimum number of buckets for NewDurationBucket.
const durationMinBuckets = 3

// NewDurationBucketer returns a Bucketer well-suited for measuring durations in
// nanoseconds. Useful for NewTimerMetric.
// minDuration and maxDuration are conservative estimates of the minimum and
// maximum durations expected to be accurately measured by the Bucketer.
func NewDurationBucketer(numFiniteBuckets int, minDuration, maxDuration time.Duration) Bucketer {
	if numFiniteBuckets < durationMinBuckets {
		panic(fmt.Sprintf("duration bucketer must have at least %d buckets, got %d", durationMinBuckets, numFiniteBuckets))
	}
	minNs := minDuration.Nanoseconds()
	exponentCoversNs := float64(maxDuration.Nanoseconds()-int64(numFiniteBuckets-durationMinBuckets)*minNs) / float64(minNs)
	exponent := math.Log(exponentCoversNs) / math.Log(float64(numFiniteBuckets-durationMinBuckets))
	minNs = int64(float64(minNs) / exponent)
	return NewExponentialBucketer(numFiniteBuckets, uint64(minNs), float64(minNs), exponent)
}

// TimerMetric wraps a distribution metric with convenience functions for
// latency measurements, which is a popular specialization of distribution
// metrics.
type TimerMetric struct {
	DistributionMetric
}

// NewTimerMetric provides a convenient way to measure latencies.
// The arguments are the same as `NewDistributionMetric`, except:
// - `nanoBucketer`: Same as `NewDistribution`'s `bucketer`, expected to hold
//                   durations in nanoseconds. Adjust parameters accordingly.
//                   NewDurationBucketer may be helpful here.
func NewTimerMetric(name string, nanoBucketer Bucketer, description string, fields ...Field) (*TimerMetric, error) {
	distrib, err := NewDistributionMetric(name, false, nanoBucketer, pb.MetricMetadata_UNITS_NANOSECONDS, description, fields...)
	if err != nil {
		return nil, err
	}
	return &TimerMetric{
		DistributionMetric: *distrib,
	}, nil
}

// MustRegisterTimerMetric creates and registers a timer metric.
// If an error occurs, it panics.
func MustRegisterTimerMetric(name string, nanoBucketer Bucketer, description string, fields ...Field) *TimerMetric {
	timer, err := NewTimerMetric(name, nanoBucketer, description, fields...)
	if err != nil {
		panic(err)
	}
	return timer
}

// TimedOperation is used by TimerMetric to keep track of the time elapsed
// between an operation starting and stopping.
type TimedOperation struct {
	// metric is a reference to the timer metric for the operation.
	metric *TimerMetric

	// partialFields is a prefix of the fields used in this operation.
	// The rest of the fields is provided in TimedOperation.Finish.
	partialFields []string

	// startedNs is the number of nanoseconds measured in TimerMetric.Start().
	startedNs int64
}

// Start starts a timer measurement for the given combination of fields.
// It returns a TimedOperation which can be passed around as necessary to
// measure the duration of the operation.
// Once the operation is finished, call Finish on the TimedOperation.
// The fields passed to Start may be partially specified; if so, the remaining
// fields must be passed to TimedOperation.Finish. This is useful for cases
// where which path an operation took is only known after it happens. This
// path can be part of the fields passed to Finish.
// +checkescape:all
//go:nosplit
func (t *TimerMetric) Start(fields ...string) TimedOperation {
	return TimedOperation{
		metric:        t,
		partialFields: fields,
		startedNs:     CheapNowNano(),
	}
}

// Finish marks an operation as finished and records its duration.
// `extraFields` is the rest of the fields appended to the fields passed to
// `TimerMetric.Start`. The concatenation of these two must be the exact
// number of fields that the underlying metric has.
// +checkescape:all
//go:nosplit
func (o TimedOperation) Finish(extraFields ...string) {
	ended := CheapNowNano()
	fieldKey := o.metric.fieldsToKey.lookupConcat(o.partialFields, extraFields)
	o.metric.addSampleByKey(ended-o.startedNs, fieldKey)
}

// stageTiming contains timing data for an initialization stage.
type stageTiming struct {
	stage   InitStage
	started time.Time
	// ended is the zero time when the stage has not ended yet.
	ended time.Time
}

// inProgress returns whether this stage hasn't ended yet.
func (s stageTiming) inProgress() bool {
	return !s.started.IsZero() && s.ended.IsZero()
}

// metricSet holds metric data.
type metricSet struct {
	// Map of uint64 metrics.
	uint64Metrics map[string]customUint64Metric

	// Map of distribution metrics.
	distributionMetrics map[string]*DistributionMetric

	// mu protects the fields below.
	mu sync.RWMutex

	// Information about the stages reached by the Sentry. Only appended to, so
	// reading a shallow copy of the slice header concurrently is safe.
	finished []stageTiming

	// The current stage in progress.
	currentStage stageTiming
}

// makeMetricSet returns a new metricSet.
func makeMetricSet() metricSet {
	return metricSet{
		uint64Metrics:       make(map[string]customUint64Metric),
		distributionMetrics: make(map[string]*DistributionMetric),
		finished:            make([]stageTiming, 0, len(allStages)),
	}
}

// Values returns a snapshot of all values in m.
func (m *metricSet) Values() metricValues {
	m.mu.Lock()
	stages := m.finished[:]
	m.mu.Unlock()

	vals := metricValues{
		uint64Metrics:            make(map[string]interface{}, len(m.uint64Metrics)),
		distributionMetrics:      make(map[string]map[string][]uint64, len(m.distributionMetrics)),
		distributionTotalSamples: make(map[string]map[string]uint64, len(m.distributionMetrics)),
		stages:                   stages,
	}
	for k, v := range m.uint64Metrics {
		fields := v.metadata.GetFields()
		switch len(fields) {
		case 0:
			vals.uint64Metrics[k] = v.value()
		case 1:
			values := fields[0].GetAllowedValues()
			fieldsMap := make(map[string]uint64)
			for _, fieldValue := range values {
				fieldsMap[fieldValue] = v.value(fieldValue)
			}
			vals.uint64Metrics[k] = fieldsMap
		default:
			panic(fmt.Sprintf("Unsupported number of metric fields: %d", len(fields)))
		}
	}
	for name, metric := range m.distributionMetrics {
		fieldKeysToValues := make(map[string][]uint64, len(metric.samples))
		fieldKeysToTotalSamples := make(map[string]uint64, len(metric.samples))
		for fieldKey, samples := range metric.samples {
			samplesSnapshot := snapshotDistribution(samples)
			totalSamples := uint64(0)
			for _, bucket := range samplesSnapshot {
				totalSamples += bucket
			}
			if totalSamples == 0 {
				// No samples recorded for this combination of field, so leave
				// the maps for this fieldKey as nil. This lessens the memory cost
				// of distributions with unused field combinations.
				fieldKeysToTotalSamples[fieldKey] = 0
				fieldKeysToValues[fieldKey] = nil
			} else {
				fieldKeysToTotalSamples[fieldKey] = totalSamples
				fieldKeysToValues[fieldKey] = samplesSnapshot
			}
		}
		vals.distributionMetrics[name] = fieldKeysToValues
		vals.distributionTotalSamples[name] = fieldKeysToTotalSamples
	}
	return vals
}

// metricValues contains a copy of the values of all metrics.
type metricValues struct {
	// uint64Metrics is a map of uint64 metrics,
	// with key as metric name. Value can be either uint64, or map[string]uint64
	// to support metrics with one field.
	uint64Metrics map[string]interface{}

	// distributionMetrics is a map of distribution metrics.
	// The first key level is the metric name.
	// The second key level is the concatenated view of the fields.
	// The value is the number of samples in each bucket of the distribution,
	// with the first (0-th) element being the underflow bucket and the last
	// element being the "infinite" (overflow) bucket.
	distributionMetrics map[string]map[string][]uint64

	// distributionTotalSamples is the total number of samples for each
	// distribution metric and field values.
	// It allows performing a quick diff between snapshots without having to
	// iterate over all the buckets individually, so that distributions with
	// no new samples are not retransmitted.
	distributionTotalSamples map[string]map[string]uint64

	// Information on when initialization stages were reached. Does not include
	// the currently-ongoing stage, if any.
	stages []stageTiming
}

var (
	// emitMu protects metricsAtLastEmit and ensures that all emitted
	// metrics are strongly ordered (older metrics are never emitted after
	// newer metrics).
	emitMu sync.Mutex

	// metricsAtLastEmit contains the state of the metrics at the last emit event.
	metricsAtLastEmit metricValues
)

// EmitMetricUpdate emits a MetricUpdate over the event channel.
//
// Only metrics that have changed since the last call are emitted.
//
// EmitMetricUpdate is thread-safe.
//
// Preconditions:
// * Initialize has been called.
func EmitMetricUpdate() {
	emitMu.Lock()
	defer emitMu.Unlock()

	snapshot := allMetrics.Values()

	m := pb.MetricUpdate{}
	// On the first call metricsAtLastEmit will be empty. Include all
	// metrics then.
	for k, v := range snapshot.uint64Metrics {
		prev, ok := metricsAtLastEmit.uint64Metrics[k]
		switch t := v.(type) {
		case uint64:
			// Metric exists and value did not change.
			if ok && prev.(uint64) == t {
				continue
			}

			m.Metrics = append(m.Metrics, &pb.MetricValue{
				Name:  k,
				Value: &pb.MetricValue_Uint64Value{Uint64Value: t},
			})
		case map[string]uint64:
			for fieldValue, metricValue := range t {
				// Emit data on the first call only if the field
				// value has been incremented. For all other
				// calls, emit data if the field value has been
				// changed from the previous emit.
				if (!ok && metricValue == 0) || (ok && prev.(map[string]uint64)[fieldValue] == metricValue) {
					continue
				}

				m.Metrics = append(m.Metrics, &pb.MetricValue{
					Name:        k,
					FieldValues: []string{fieldValue},
					Value:       &pb.MetricValue_Uint64Value{Uint64Value: metricValue},
				})
			}
		}
	}
	for name, dist := range snapshot.distributionTotalSamples {
		prev, ok := metricsAtLastEmit.distributionTotalSamples[name]
		for fieldKey, currentTotal := range dist {
			if currentTotal == 0 {
				continue
			}
			if ok {
				if prevTotal, ok2 := prev[fieldKey]; ok2 && prevTotal == currentTotal {
					continue
				}
			}
			oldSamples := metricsAtLastEmit.distributionMetrics[name][fieldKey]
			var newSamples []uint64
			if oldSamples != nil {
				currentSamples := snapshot.distributionMetrics[name][fieldKey]
				numBuckets := len(currentSamples)
				newSamples = make([]uint64, numBuckets)
				for i := 0; i < numBuckets; i++ {
					newSamples[i] = currentSamples[i] - oldSamples[i]
				}
			} else {
				// oldSamples == nil means that the previous snapshot has no samples.
				// This means the delta is the current number of samples, no need for
				// a copy.
				newSamples = snapshot.distributionMetrics[name][fieldKey]
			}
			m.Metrics = append(m.Metrics, &pb.MetricValue{
				Name:        name,
				FieldValues: keyToMultiField(fieldKey),
				Value: &pb.MetricValue_DistributionValue{
					DistributionValue: &pb.Samples{
						NewSamples: newSamples,
					},
				},
			})
		}
	}

	for s := len(metricsAtLastEmit.stages); s < len(snapshot.stages); s++ {
		newStage := snapshot.stages[s]
		m.StageTiming = append(m.StageTiming, &pb.StageTiming{
			Stage: string(newStage.stage),
			Started: &timestamppb.Timestamp{
				Seconds: newStage.started.Unix(),
				Nanos:   int32(newStage.started.Nanosecond()),
			},
			Ended: &timestamppb.Timestamp{
				Seconds: newStage.ended.Unix(),
				Nanos:   int32(newStage.ended.Nanosecond()),
			},
		})
	}

	metricsAtLastEmit = snapshot
	if len(m.Metrics) == 0 && len(m.StageTiming) == 0 {
		return
	}

	if log.IsLogging(log.Debug) {
		sort.Slice(m.Metrics, func(i, j int) bool {
			return m.Metrics[i].Name < m.Metrics[j].Name
		})
		log.Debugf("Emitting metrics:")
		for _, metric := range m.Metrics {
			log.Debugf("%s: %+v", metric.Name, metric.Value)
		}
		for _, stage := range m.StageTiming {
			duration := time.Duration(stage.Ended.Seconds-stage.Started.Seconds)*time.Second + time.Duration(stage.Ended.Nanos-stage.Started.Nanos)*time.Nanosecond
			log.Debugf("Stage %s took %v", stage.GetStage(), duration)
		}
	}

	if err := eventchannel.Emit(&m); err != nil {
		log.Warningf("Unable to emit metrics: %s", err)
	}
}

// StartStage should be called when an initialization stage is started.
// It returns a function that must be called to indicate that the stage ended.
// Alternatively, future calls to StartStage will implicitly indicate that the
// previous stage ended.
// Stage information will be emitted in the next call to EmitMetricUpdate after
// a stage has ended.
//
// This function may (and is expected to) be called prior to final
// initialization of this metric library, as it has to capture early stages
// of Sentry initialization.
func StartStage(stage InitStage) func() {
	now := time.Now()
	allMetrics.mu.Lock()
	defer allMetrics.mu.Unlock()
	if allMetrics.currentStage.inProgress() {
		endStage(now)
	}
	allMetrics.currentStage.stage = stage
	allMetrics.currentStage.started = now
	return func() {
		now := time.Now()
		allMetrics.mu.Lock()
		defer allMetrics.mu.Unlock()
		// The current stage may have been ended by another call to StartStage, so
		// double-check prior to clearing the current stage.
		if allMetrics.currentStage.inProgress() && allMetrics.currentStage.stage == stage {
			endStage(now)
		}
	}
}

// endStage marks allMetrics.currentStage as ended, adding it to the list of
// finished stages. It assumes allMetrics.mu is locked.
func endStage(when time.Time) {
	allMetrics.currentStage.ended = when
	allMetrics.finished = append(allMetrics.finished, allMetrics.currentStage)
	allMetrics.currentStage = stageTiming{}
}
