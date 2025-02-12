// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package registry

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/spec"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
)

// LibGEOS is a list of native libraries for libgeos.
var LibGEOS = []string{"libgeos", "libgeos_c"}

// PrometheusNameSpace is the namespace which all metrics exposed on the roachtest
// endpoint should use.
var PrometheusNameSpace = "roachtest"

// TestSpec is a spec for a roachtest.
type TestSpec struct {
	Skip string // if non-empty, test will be skipped
	// When Skip is set, this can contain more text to be printed in the logs
	// after the "--- SKIP" line.
	SkipDetails string

	Name string
	// Owner is the name of the team responsible for signing off on failures of
	// this test that happen in the release process. This must be one of a limited
	// set of values (the keys in the roachtestTeams map).
	Owner Owner
	// The maximum duration the test is allowed to run before it is considered
	// failed. If not specified, the default timeout is 10m before the test's
	// associated cluster expires. The timeout is always truncated to 10m before
	// the test's cluster expires.
	Timeout time.Duration
	// Denotes whether the test is a roachperf benchmark. If true, the test is expected, but not required, to produce
	// artifacts in Test.PerfArtifactsDir(), which get exported to the roachperf dashboard
	// (see getPerfArtifacts() in test_runner.go).
	// N.B. performance tests may choose to _assert_ on latency, throughput, or some other perf. metric, _without_
	// exporting artifacts to the dashboard. E.g., see 'registerKVContention' and 'verifyTxnPerSecond'.
	// N.B. performance tests may have different requirements than correctness tests, e.g., machine type/architecture.
	// Thus, they must be opted into explicitly via this field.
	Benchmark bool

	// CompatibleClouds is the set of clouds this test can run on (e.g. AllClouds,
	// OnlyGCE, etc). Must be set.
	CompatibleClouds CloudSet

	// Suites is the set of suites this test is part of (e.g. Nightly, Weekly,
	// etc). Must be set, even if empty (see ManualOnly).
	Suites SuiteSet

	// Tags is a set of tags associated with the test that allow grouping
	// tests. If no tags are specified, the set ["default"] is automatically
	// given.
	Tags map[string]struct{}
	// Cluster provides the specification for the cluster to use for the test.
	Cluster spec.ClusterSpec
	// NativeLibs specifies the native libraries required to be present on
	// the cluster during execution.
	NativeLibs []string

	// UseIOBarrier controls the local-ssd-no-ext4-barrier flag passed to
	// roachprod when creating a cluster. If set, the flag is not passed, and so
	// you get durable writes. If not set (the default!), the filesystem is
	// mounted without the barrier.
	//
	// The default (false) is chosen because it the no-barrier option is needed
	// explicitly by some tests (particularly benchmarks, ironically, since they'd
	// rather measure other things than I/O) and the vast majority of other tests
	// don't care - there's no durability across machine crashes that roachtests
	// care about.
	UseIOBarrier bool

	// NonReleaseBlocker indicates that a test failure should not be tagged as a
	// release blocker. Use this for tests that are not yet stable but should
	// still be run regularly.
	NonReleaseBlocker bool

	// RequiresLicense indicates that the test requires an
	// enterprise license to run correctly. Use this to ensure
	// tests will fail-early if COCKROACH_DEV_LICENSE is not set
	// in the environment.
	RequiresLicense bool

	// EncryptionSupport encodes to what extent tests supports
	// encryption-at-rest. See the EncryptionSupport type for details.
	// Encryption support is opt-in -- i.e., if the TestSpec does not
	// pass a value to this field, it will be assumed that the test
	// cannot be run with encryption enabled.
	EncryptionSupport EncryptionSupport

	// Leases specifies the kind of leases to use for the cluster. Defaults
	// to epoch leases.
	Leases LeaseType

	// SkipPostValidations is a bit-set of post-validations that should be skipped
	// after the test completes. This is useful for tests that are known to be
	// incompatible with some validations. By default, tests will run all
	// validations.
	SkipPostValidations PostValidation

	// Run is the test function.
	Run func(ctx context.Context, t test.Test, c cluster.Cluster)

	// True iff results from this test should not be published externally,
	// e.g. to GitHub.
	RedactResults bool

	// SnapshotPrefix is set by tests that make use of volume snapshots.
	// Prefixes must be not only be unique across tests, but one cannot be a
	// prefix of another.
	//
	// TODO(irfansharif): Search by taking in the other parts of the snapshot
	// fingerprint, i.e. the node count, the version, etc. that appear in the
	// infix. This will get rid of this awkward prefix naming restriction.
	SnapshotPrefix string

	// ExtraLabels are test-specific labels that will be added to the Github
	// issue created when a failure occurs, in addition to default labels.
	ExtraLabels []string
}

// PostValidation is a type of post-validation that runs after a test completes.
type PostValidation int

const (
	// PostValidationReplicaDivergence detects replica divergence (i.e. ranges in
	// which replicas have arrived at the same log position with different
	// states).
	PostValidationReplicaDivergence PostValidation = 1 << iota
	// PostValidationInvalidDescriptors checks if there exists any descriptors in
	// the crdb_internal.invalid_objects virtual table.
	PostValidationInvalidDescriptors
	// PostValidationNoDeadNodes checks if there are any dead nodes in the cluster.
	PostValidationNoDeadNodes
)

// MatchType is the type of match a file has to a TestFilter.
type MatchType int

const (
	// Matched means that the file passes the filter and the tags.
	Matched MatchType = iota
	// FailedFilter means that the file fails the filter.
	FailedFilter
	// FailedTags means that the file passed the filter but failed the tags
	// match.
	FailedTags
)

// Match returns Matched if the filter matches the test. If the filter does
// not match the test because the tag filter does not match, the test is
// marked as FailedTags.
func (t *TestSpec) Match(filter *TestFilter) MatchType {
	if !filter.Name.MatchString(t.Name) {
		return FailedFilter
	}

	if len(filter.Tags) == 0 {
		return Matched
	}

	for tag := range filter.Tags {
		// If the tag is a single CSV e.g. "foo,bar,baz", we match all the tags
		if matchesAll(t.Tags, strings.Split(tag, ",")) {
			return Matched
		}
	}

	return FailedTags
}

// PromSub replaces all non prometheus friendly chars with "_". Note,
// before creating a metric, read up on prom metric naming conventions:
// https://prometheus.io/docs/practices/naming/
func PromSub(raw string) string {
	invalidPromRE := regexp.MustCompile("[^a-zA-Z0-9_]")
	return invalidPromRE.ReplaceAllLiteralString(raw, "_")
}

func matchesAll(testTags map[string]struct{}, filterTags []string) bool {
	for _, tag := range filterTags {
		negate := false
		if tag[0] == '!' {
			negate = true
			tag = tag[1:]
		}
		_, tagExists := testTags[tag]

		if negate == tagExists {
			return false
		}
	}
	return true
}

// Tags returns a set of strings.
func Tags(values ...string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, s := range values {
		set[s] = struct{}{}
	}
	return set
}

// LeaseType specifies the type of leases to use for the cluster.
type LeaseType int

func (l LeaseType) String() string {
	switch l {
	case DefaultLeases:
		return "default"
	case EpochLeases:
		return "epoch"
	case ExpirationLeases:
		return "expiration"
	case MetamorphicLeases:
		return "metamorphic"
	default:
		return fmt.Sprintf("leasetype-%d", l)
	}
}

const (
	// DefaultLeases uses the default cluster lease type.
	DefaultLeases = LeaseType(iota)
	// EpochLeases uses epoch leases where possible.
	EpochLeases
	// ExpirationLeases uses expiration leases for all ranges.
	ExpirationLeases
	// MetamorphicLeases randomly chooses epoch or expiration
	// leases (across the entire cluster)
	MetamorphicLeases
)

var allClouds = []string{spec.Local, spec.GCE, spec.AWS, spec.Azure}

// CloudSet represents a set of clouds.
//
// Instances of CloudSet are immutable. The uninitialized (zero) value is not
// valid.
type CloudSet struct {
	// m contains only values from allClouds.
	m map[string]struct{}
}

// AllClouds contains all clouds.
var AllClouds = Clouds(allClouds...)

// AllExceptLocal contains all clouds except Local.
var AllExceptLocal = AllClouds.NoLocal()

// AllExceptAWS contains all clouds except AWS.
var AllExceptAWS = AllClouds.NoAWS()

// OnlyGCE contains only the GCE cloud.
var OnlyGCE = Clouds(spec.GCE)

// OnlyLocal contains only the GCE cloud.
var OnlyLocal = Clouds(spec.Local)

// Clouds creates a CloudSet for the given clouds. Cloud names must be one of:
// spec.Local, spec.GCE, spec.AWS, spec.Azure.
func Clouds(clouds ...string) CloudSet {
	assertValidValues(allClouds, clouds...)
	return CloudSet{m: addToSet(nil, clouds...)}
}

// NoLocal removes the Local cloud and returns the new set.
func (cs CloudSet) NoLocal() CloudSet {
	return CloudSet{m: removeFromSet(cs.m, spec.Local)}
}

// NoAWS removes the AWS cloud and returns the new set.
func (cs CloudSet) NoAWS() CloudSet {
	return CloudSet{m: removeFromSet(cs.m, spec.AWS)}
}

// NoAzure removes the Azure cloud and returns the new set.
func (cs CloudSet) NoAzure() CloudSet {
	return CloudSet{m: removeFromSet(cs.m, spec.Azure)}
}

// Contains returns true if the set contains the given cloud.
func (cs CloudSet) Contains(cloud string) bool {
	cs.AssertInitialized()
	_, ok := cs.m[cloud]
	return ok
}

func (cs CloudSet) String() string {
	cs.AssertInitialized()
	return setToString(allClouds, cs.m)
}

// AssertInitialized panics if the CloudSet is the zero value.
func (cs CloudSet) AssertInitialized() {
	if cs.m == nil {
		panic("CloudSet not initialized")
	}
}

// Suite names.
const (
	Nightly               = "nightly"
	Weekly                = "weekly"
	ReleaseQualification  = "release_qualification"
	ORM                   = "orm"
	Driver                = "driver"
	Tool                  = "tool"
	Smoketest             = "smoketest"
	Quick                 = "quick"
	Fixtures              = "fixtures"
	Pebble                = "pebble"
	PebbleNightlyWrite    = "pebble_nightly_write"
	PebbleNightlyYCSB     = "pebble_nightly_ycsb"
	PebbleNightlyYCSBRace = "pebble_nightly_ycsb_race"
	Roachtest             = "roachtest"
)

var allSuites = []string{
	Nightly, Weekly, ReleaseQualification, ORM, Driver, Tool, Smoketest, Quick, Fixtures,
	Pebble, PebbleNightlyWrite, PebbleNightlyYCSB, PebbleNightlyYCSBRace, Roachtest,
}

// SuiteSet represents a set of suites.
//
// Instances of SuiteSet are immutable. The uninitialized (zero) value is not
// valid.
type SuiteSet struct {
	// m contains only values from allSuites.
	m map[string]struct{}
}

// ManualOnly is used for tests that are not part of any suite; these tests are
// only run manually.
var ManualOnly = Suites()

// Suites creates a SuiteSet with the given suites. Only the constants above are
// valid values.
func Suites(suites ...string) SuiteSet {
	assertValidValues(allSuites, suites...)
	return SuiteSet{m: addToSet(nil, suites...)}
}

// Contains returns true if the set contains the given suite.
func (ss SuiteSet) Contains(suite string) bool {
	ss.AssertInitialized()
	_, ok := ss.m[suite]
	return ok
}

func (ss SuiteSet) String() string {
	return setToString(allSuites, ss.m)
}

// AssertInitialized panics if the SuiteSet is the zero value.
func (ss SuiteSet) AssertInitialized() {
	if ss.m == nil {
		panic("SuiteSet not initialized")
	}
}

// CrossCheckTags verifies that the CompatibleClouds and Suites values match those of the tags.
func (t *TestSpec) CrossCheckTags() {
	// Check that CompatibleClouds + Suites would make the same determination as
	// tags for what test to run for Nightly/Weekly and GCE/AWS.
	var expected, actual struct {
		nightlyGCE bool
		weeklyGCE  bool
		nightlyAWS bool
		weeklyAWS  bool
	}

	expected.nightlyGCE = matchesAll(t.Tags, []string{"default"}) || matchesAll(t.Tags, []string{"aws"})
	expected.weeklyGCE = matchesAll(t.Tags, []string{"weekly"})
	expected.nightlyAWS = matchesAll(t.Tags, []string{"aws"})
	expected.weeklyAWS = matchesAll(t.Tags, []string{"aws-weekly"})

	actual.nightlyGCE = t.Suites.Contains(Nightly) && t.CompatibleClouds.Contains(spec.GCE)
	actual.weeklyGCE = t.Suites.Contains(Weekly) && t.CompatibleClouds.Contains(spec.GCE)
	actual.nightlyAWS = t.Suites.Contains(Nightly) && t.CompatibleClouds.Contains(spec.AWS)
	actual.weeklyAWS = t.Suites.Contains(Weekly) && t.CompatibleClouds.Contains(spec.AWS)

	if actual != expected {
		panic(fmt.Sprintf("CompatibleClouds/Suites inconsistent with Tags\nexpected: %#v\nactual:   %#v\nclouds: %s  suites:%s  tags:%v\n", expected, actual, t.CompatibleClouds, t.Suites, t.Tags))
	}

	otherSuiteTags := fmt.Sprintf("%v", removeFromSet(t.Tags, "default", "weekly", "aws-weekly", "aws", "owner-"+string(t.Owner)))
	otherSuites := fmt.Sprintf("%v", removeFromSet(t.Suites.m, Weekly, Nightly))
	if otherSuiteTags != otherSuites && !(otherSuiteTags == "map[manual:{}]" && otherSuites == "map[]") {
		panic(fmt.Sprintf("Suites inconsistent with Tags\nexpected: %v\nactual:   %v\nsuites:%s  tags:%v\n", otherSuiteTags, otherSuites, t.Suites, t.Tags))
	}
}

// assertValidValues asserts that the given values exist in the validValues slice.
func assertValidValues(validValues []string, values ...string) {
	for _, v := range values {
		found := false
		for _, valid := range validValues {
			if valid == v {
				found = true
				break
			}
		}
		if !found {
			panic(fmt.Sprintf("invalid value %q (valid values: %v)", v, validValues))
		}
	}
}

// addToSet returns a new set that is the initial set with the given values added.
func addToSet(initial map[string]struct{}, values ...string) map[string]struct{} {
	m := make(map[string]struct{})
	for k := range initial {
		m[k] = struct{}{}
	}
	for _, v := range values {
		m[v] = struct{}{}
	}
	return m
}

// removeFromSet returns a new set that is the initial set with the given values removed.
func removeFromSet(initial map[string]struct{}, values ...string) map[string]struct{} {
	m := make(map[string]struct{})
	for k := range initial {
		m[k] = struct{}{}
	}
	for _, v := range values {
		delete(m, v)
	}
	return m
}

// setToString returns the elements of a set, in the relative order in which they appear
// in validValues. Returns "<none>" if the set is empty.
func setToString(validValues []string, m map[string]struct{}) string {
	var elems []string
	for _, v := range validValues {
		if _, ok := m[v]; ok {
			elems = append(elems, v)
		}
	}
	if len(elems) == 0 {
		return "<none>"
	}
	return strings.Join(elems, ",")
}
