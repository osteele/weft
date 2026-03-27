package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/osteele/weft/internal/db"
)

func TestGetTargetJobPrefersHighlightedInDetailsTab(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "host-a"},
		{ID: 2, Host: "host-b"},
	}

	// Create a job list with the jobs
	jobList := list.New(JobsToListItems(jobs), NewJobDelegate(), 80, 10)
	jobList.Select(0)

	m := Model{
		jobs:               jobs,
		jobList:            jobList,
		selectedJob:        jobs[1],
		detailTab:          DetailTabDetails,
		jobSelectionActive: true,
	}

	if got := m.getTargetJob(); got == nil || got.ID != 1 {
		t.Fatalf("expected highlighted job 1 in Details tab, got %+v", got)
	}

	m.detailTab = DetailTabLogs
	if got := m.getTargetJob(); got == nil || got.ID != 2 {
		t.Fatalf("expected selected log job 2 in Logs tab, got %+v", got)
	}
}

func TestJobMatchesHostFilterRecentIncludesCloudAndUnplacedJobs(t *testing.T) {
	m := Model{
		jobHostFilterMode: hostFilterRecent,
		hostSyncTimes: map[string]time.Time{
			"host-a": time.Now(),
		},
	}

	cloudInstanceID := int64(32708838)

	tests := []struct {
		name string
		job  *db.Job
		want bool
	}{
		{
			name: "recently synced host remains visible",
			job:  &db.Job{Host: "host-a"},
			want: true,
		},
		{
			name: "stale normal host remains hidden",
			job:  &db.Job{Host: "host-b"},
			want: false,
		},
		{
			name: "cloud instance job is visible",
			job:  &db.Job{Host: "vastai:32708838", LaunchID: &cloudInstanceID},
			want: true,
		},
		{
			name: "cloud host string is visible even without instance id",
			job:  &db.Job{Host: "runpod:456"},
			want: true,
		},
		{
			name: "unplaced job is visible",
			job:  &db.Job{Host: ""},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.jobMatchesHostFilter(tt.job); got != tt.want {
				t.Fatalf("jobMatchesHostFilter(%+v) = %v, want %v", tt.job, got, tt.want)
			}
		})
	}
}

func TestHostFromCachedInfoSetsLastCheck(t *testing.T) {
	// Simulate cached host info with a timestamp from 2 hours ago
	oldTimestamp := time.Now().Add(-2 * time.Hour).Unix()

	cached := &db.CachedHostInfo{
		Name:        "test-host",
		LastUpdated: oldTimestamp,
		Arch:        "Linux x86_64",
	}

	host := hostFromCachedInfo(cached)

	// Verify LastCheck is set correctly from LastUpdated
	if host.LastCheck.IsZero() {
		t.Fatal("LastCheck should not be zero")
	}

	expectedTime := time.Unix(oldTimestamp, 0)
	if !host.LastCheck.Equal(expectedTime) {
		t.Errorf("LastCheck = %v, want %v", host.LastCheck, expectedTime)
	}

	// Verify Status is Unknown (not online)
	if host.Status != HostStatusUnknown {
		t.Errorf("Status = %v, want HostStatusUnknown", host.Status)
	}
}

// TestDisconnectedHostFromFreshCacheShowsGray simulates the exact scenario:
// - Host cache is less than 24h old (considered "fresh", no fetch triggered)
// - But host was last seen > 30 minutes ago
// - Jobs on this host should be dimmed (isHostDisconnectedLong = true)
func TestDisconnectedHostFromFreshCacheShowsGray(t *testing.T) {
	// Simulate: cache is 21 hours old (< 24h cache duration, so "fresh")
	// But last successful connection was 21 hours ago (> 30 min threshold)
	lastSeenTime := time.Now().Add(-21 * time.Hour)

	// Create a host as it would be created from fresh cache (no fetch triggered)
	// This simulates what happens when cacheAge < hostCacheDuration
	host := &Host{
		Name:      "host-alpha",
		Status:    HostStatusUnknown, // From hostFromCachedInfo
		LastCheck: lastSeenTime,      // From cached.LastUpdated
	}

	m := Model{hosts: []*Host{host}}
	job := &db.Job{Host: "host-alpha"}

	// This should return true - host hasn't been seen in > 30 minutes
	if !m.isHostDisconnectedLong(job) {
		t.Errorf("isHostDisconnectedLong() = false, want true for host not seen in %v", time.Since(lastSeenTime))
	}
}

// TestJobsOnMissingHostsNotDimmed verifies behavior when a job references
// a host that isn't in the hosts list (e.g., hosts not yet loaded)
func TestJobsOnMissingHostsNotDimmed(t *testing.T) {
	// Empty hosts list
	m := Model{hosts: []*Host{}}
	job := &db.Job{Host: "host-gamma"}

	// Should return false when host isn't in the list
	if m.isHostDisconnectedLong(job) {
		t.Error("isHostDisconnectedLong() should return false for hosts not in list")
	}
}

// TestMultipleHostsWithMixedStatus tests that only disconnected hosts
// cause their jobs to be dimmed, while online hosts don't
func TestMultipleHostsWithMixedStatus(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-2 * time.Hour)

	hosts := []*Host{
		{Name: "host-beta", Status: HostStatusOnline, LastCheck: now},
		{Name: "host-alpha", Status: HostStatusUnknown, LastCheck: oldTime},
		{Name: "host-gamma", Status: HostStatusOffline, LastCheck: oldTime},
	}

	m := Model{hosts: hosts}

	// Job on online host - NOT dimmed
	jobCool30 := &db.Job{Host: "host-beta"}
	if m.isHostDisconnectedLong(jobCool30) {
		t.Error("Job on cool30 (online) should NOT be dimmed")
	}

	// Job on unknown host with old LastCheck - dimmed
	jobCool100 := &db.Job{Host: "host-alpha"}
	if !m.isHostDisconnectedLong(jobCool100) {
		t.Error("Job on cool100 (unknown, old LastCheck) SHOULD be dimmed")
	}

	// Job on offline host with old LastCheck - dimmed
	jobStudio := &db.Job{Host: "host-gamma"}
	if !m.isHostDisconnectedLong(jobStudio) {
		t.Error("Job on studio (offline, old LastCheck) SHOULD be dimmed")
	}
}

// TestHostFromCachedInfoIntegration tests the complete flow:
// 1. Cache has host data with old LastUpdated
// 2. hostFromCachedInfo creates host with correct LastCheck
// 3. isHostDisconnectedLong returns true
func TestHostFromCachedInfoIntegration(t *testing.T) {
	// Simulate what the database would return - host last seen 21 hours ago
	// (like cool100 in the actual database)
	oldTimestamp := time.Now().Add(-21 * time.Hour).Unix()

	cached := &db.CachedHostInfo{
		Name:        "host-alpha",
		LastUpdated: oldTimestamp,
		Arch:        "Linux x86_64",
	}

	// Step 1: Create host from cached info (as hostsLoadedMsg handler does)
	host := hostFromCachedInfo(cached)

	// Verify host was created correctly
	t.Logf("Host created: Status=%v, LastCheck=%v (ago: %v)",
		host.Status, host.LastCheck, time.Since(host.LastCheck))

	if host.Status != HostStatusUnknown {
		t.Errorf("Expected HostStatusUnknown, got %v", host.Status)
	}

	expectedLastCheck := time.Unix(oldTimestamp, 0)
	if !host.LastCheck.Equal(expectedLastCheck) {
		t.Errorf("LastCheck = %v, want %v", host.LastCheck, expectedLastCheck)
	}

	// Step 2: Create model with this host
	m := Model{hosts: []*Host{host}}

	// Step 3: Check if job on this host is detected as disconnected
	job := &db.Job{Host: "host-alpha"}
	disconnected := m.isHostDisconnectedLong(job)

	t.Logf("isHostDisconnectedLong returned: %v", disconnected)

	if !disconnected {
		t.Errorf("Expected job on cool100 to be marked as disconnected (host.Status=%v, LastCheck=%v, ago=%v)",
			host.Status, host.LastCheck, time.Since(host.LastCheck))
	}
}

// TestRealDatabaseHostCaching tests with the actual user database if available
func TestRealDatabaseHostCaching(t *testing.T) {
	// Try to open the real database
	database, err := db.Open()
	if err != nil {
		t.Skip("Could not open database:", err)
	}
	defer database.Close()

	// Load cached info for cool100
	cachedInfo, err := db.LoadCachedHostInfo(database, "host-alpha")
	if err != nil {
		t.Fatalf("LoadCachedHostInfo error: %v", err)
	}
	if cachedInfo == nil {
		t.Skip("cool100 not in cache")
	}

	t.Logf("cool100 cache: LastUpdated=%d (%v), ago=%v",
		cachedInfo.LastUpdated,
		time.Unix(cachedInfo.LastUpdated, 0),
		time.Since(time.Unix(cachedInfo.LastUpdated, 0)))

	// Create host from cached info
	host := hostFromCachedInfo(cachedInfo)
	t.Logf("Host from cache: Status=%v, LastCheck=%v", host.Status, host.LastCheck)

	// Check cache age calculation
	cacheAge := time.Since(time.Unix(cachedInfo.LastUpdated, 0))
	t.Logf("Cache age: %v, threshold: %v", cacheAge, 24*time.Hour)
	t.Logf("Cache stale (>24h)? %v", cacheAge > 24*time.Hour)

	// Test isHostDisconnectedLong
	m := Model{
		hosts:             []*Host{host},
		hostCacheDuration: 24 * time.Hour,
	}
	job := &db.Job{Host: "host-alpha"}

	disconnected := m.isHostDisconnectedLong(job)
	t.Logf("isHostDisconnectedLong for cool100: %v", disconnected)

	// Should be disconnected if LastCheck > 30 minutes ago
	if time.Since(host.LastCheck) > 30*time.Minute && !disconnected {
		t.Errorf("Expected cool100 to be marked as disconnected")
	}
}

func TestIsHostDisconnectedLong(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-45 * time.Minute)    // 45 minutes ago (> 30 min threshold)
	recentTime := now.Add(-10 * time.Minute) // 10 minutes ago (< 30 min threshold)

	tests := []struct {
		name     string
		hosts    []*Host
		job      *db.Job
		expected bool
	}{
		{
			name: "host online - not disconnected",
			hosts: []*Host{
				{Name: "host-beta", Status: HostStatusOnline, LastCheck: oldTime},
			},
			job:      &db.Job{Host: "host-beta"},
			expected: false,
		},
		{
			name: "host offline with old LastCheck - disconnected",
			hosts: []*Host{
				{Name: "host-alpha", Status: HostStatusOffline, LastCheck: oldTime},
			},
			job:      &db.Job{Host: "host-alpha"},
			expected: true,
		},
		{
			name: "host offline with recent LastCheck - not disconnected",
			hosts: []*Host{
				{Name: "host-alpha", Status: HostStatusOffline, LastCheck: recentTime},
			},
			job:      &db.Job{Host: "host-alpha"},
			expected: false,
		},
		{
			name: "host unknown with old LastCheck - disconnected",
			hosts: []*Host{
				{Name: "host-gamma", Status: HostStatusUnknown, LastCheck: oldTime},
			},
			job:      &db.Job{Host: "host-gamma"},
			expected: true,
		},
		{
			name: "host checking with old LastCheck - disconnected",
			hosts: []*Host{
				{Name: "host-gamma", Status: HostStatusChecking, LastCheck: oldTime},
			},
			job:      &db.Job{Host: "host-gamma"},
			expected: true,
		},
		{
			name: "host not in list - not disconnected",
			hosts: []*Host{
				{Name: "host-beta", Status: HostStatusOnline, LastCheck: now},
			},
			job:      &db.Job{Host: "unknown-host"},
			expected: false,
		},
		{
			name: "host offline with zero LastCheck - not disconnected",
			hosts: []*Host{
				{Name: "host-alpha", Status: HostStatusOffline, LastCheck: time.Time{}},
			},
			job:      &db.Job{Host: "host-alpha"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{hosts: tt.hosts}
			got := m.isHostDisconnectedLong(tt.job)
			if got != tt.expected {
				t.Errorf("isHostDisconnectedLong() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestNaturalSortStrings(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "numeric suffixes",
			input:    []string{"host-alpha", "host-beta", "cool10", "cool2"},
			expected: []string{"cool2", "cool10", "host-alpha", "host-beta"},
		},
		{
			name:     "mixed hosts",
			input:    []string{"host-gamma", "host-alpha", "host-beta", "alpha"},
			expected: []string{"alpha", "host-alpha", "host-beta", "host-gamma"},
		},
		{
			name:     "pure alphabetic",
			input:    []string{"charlie", "alpha", "bravo"},
			expected: []string{"alpha", "bravo", "charlie"},
		},
		{
			name:     "numeric only",
			input:    []string{"100", "30", "10", "2"},
			expected: []string{"2", "10", "30", "100"},
		},
		{
			name:     "case insensitive",
			input:    []string{"Cool30", "cool10", "COOL20"},
			expected: []string{"cool10", "COOL20", "Cool30"},
		},
		{
			name:     "prefixes vary",
			input:    []string{"server2", "host10", "server1", "host2"},
			expected: []string{"host2", "host10", "server1", "server2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := make([]string, len(tt.input))
			copy(input, tt.input)
			naturalSortStrings(input)
			for i, got := range input {
				if got != tt.expected[i] {
					t.Errorf("naturalSortStrings() at index %d = %q, want %q (full result: %v)", i, got, tt.expected[i], input)
				}
			}
		})
	}
}
