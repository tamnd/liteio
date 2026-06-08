// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
)

// fakeInfo is a static topology and usage source so the info and health endpoints
// can be tested without standing up a real object layer.
type fakeInfo struct {
	si object.StorageInfo
	du object.DiskUsage
}

func (f fakeInfo) StorageInfo() object.StorageInfo            { return f.si }
func (f fakeInfo) DiskUsage(context.Context) object.DiskUsage { return f.du }

// healthySet is one set of n drives, parity m, all online.
func healthySet(n, m int) object.SetInfo {
	set := object.SetInfo{Parity: m}
	for range n {
		set.Drives = append(set.Drives, object.DriveInfo{Endpoint: "drive", Online: true})
	}
	return set
}

// degradedSet is one set of n drives, parity m, with off of them offline.
func degradedSet(n, m, off int) object.SetInfo {
	set := healthySet(n, m)
	for i := range min(off, n) {
		set.Drives[i].Online = false
	}
	return set
}

func topology(sets ...object.SetInfo) object.StorageInfo {
	return object.StorageInfo{
		DeploymentID: "0011223344556677",
		Pools:        []object.PoolInfo{{Sets: sets}},
	}
}

// newInfoHarness builds a harness whose admin server reports the given topology.
func newInfoHarness(t *testing.T, si object.StorageInfo) *harness {
	t.Helper()
	return newInfoUsageHarness(t, si, object.DiskUsage{})
}

// newInfoUsageHarness is newInfoHarness with an explicit capacity rollup.
func newInfoUsageHarness(t *testing.T, si object.StorageInfo, du object.DiskUsage) *harness {
	t.Helper()
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	if err := store.AddUser(auth.User{AccessKey: viewerCreds.AccessKey, SecretKey: viewerCreds.SecretKey, Policies: []string{"readonly"}}); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	creds := auth.NewStaticStore(adminCreds, viewerCreds)
	server := NewServer(store, creds,
		WithInfo(fakeInfo{si: si, du: du}),
		WithVersion("v1.2.3"),
		WithClock(func() time.Time { return time.Now().UTC() }),
	)
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv}
}

func TestServerInfoReportsTopology(t *testing.T) {
	h := newInfoHarness(t, topology(healthySet(6, 2), degradedSet(4, 2, 1)))
	res := h.admin(http.MethodGet, "/liteio/admin/v1/info", nil)
	mustStatus(t, res, http.StatusOK)

	info := decode[serverInfoResponse](t, res)
	if info.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", info.Version)
	}
	if info.DeploymentID != "0011223344556677" {
		t.Errorf("deploymentId = %q", info.DeploymentID)
	}
	if info.PoolCount != 1 || info.SetCount != 2 {
		t.Errorf("pools=%d sets=%d, want 1/2", info.PoolCount, info.SetCount)
	}
	if info.DriveCount != 10 || info.OnlineDrives != 9 {
		t.Errorf("drives=%d online=%d, want 10/9", info.DriveCount, info.OnlineDrives)
	}
	// First set is healthy and available; second is degraded but still available
	// (3 of 4 online, read quorum 2).
	s0, s1 := info.Pools[0].Sets[0], info.Pools[0].Sets[1]
	if !s0.Healthy || !s0.Available || s0.ReadQuorum != 4 {
		t.Errorf("set0 = %+v", s0)
	}
	if s1.Healthy || !s1.Available || s1.OnlineCount != 3 || s1.ReadQuorum != 2 {
		t.Errorf("set1 = %+v", s1)
	}
}

func TestServerInfoReportsCapacity(t *testing.T) {
	// Two sets, each with its own capacity; the endpoint must surface both per-set
	// figures and the deployment rollup, lined up by walk order.
	du := object.DiskUsage{
		Sets: []object.SetUsage{
			{RawTotal: 6000, RawFree: 3000, UsableTotal: 4000, UsableFree: 2000, DriveCount: 6, DrivesReporting: 6},
			{RawTotal: 4000, RawFree: 1000, UsableTotal: 2000, UsableFree: 500, DriveCount: 4, DrivesReporting: 3},
		},
		RawTotal: 10000, RawFree: 4000, UsableTotal: 6000, UsableFree: 2500,
	}
	h := newInfoUsageHarness(t, topology(healthySet(6, 2), degradedSet(4, 2, 1)), du)
	info := decode[serverInfoResponse](t, h.admin(http.MethodGet, "/liteio/admin/v1/info", nil))

	if info.RawCapacity != 10000 || info.UsableCapacity != 6000 || info.UsableFree != 2500 {
		t.Errorf("rollup capacity = %+v", info)
	}
	s0, s1 := info.Pools[0].Sets[0], info.Pools[0].Sets[1]
	if s0.RawCapacity != 6000 || s0.UsableCapacity != 4000 || s0.DrivesReporting != 6 {
		t.Errorf("set0 capacity = %+v", s0)
	}
	if s1.RawCapacity != 4000 || s1.UsableFree != 500 || s1.DrivesReporting != 3 {
		t.Errorf("set1 capacity = %+v", s1)
	}
}

func TestHealthStatus(t *testing.T) {
	cases := []struct {
		name string
		si   object.StorageInfo
		want string
	}{
		{"all healthy", topology(healthySet(6, 2), healthySet(4, 2)), "healthy"},
		{"one degraded", topology(healthySet(6, 2), degradedSet(4, 2, 1)), "degraded"},
		{"one unavailable", topology(healthySet(6, 2), degradedSet(4, 2, 3)), "unavailable"},
		{"unavailable wins over degraded", topology(degradedSet(6, 2, 1), degradedSet(4, 2, 3)), "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newInfoHarness(t, tc.si)
			res := h.admin(http.MethodGet, "/liteio/admin/v1/health", nil)
			mustStatus(t, res, http.StatusOK)
			got := decode[healthResponse](t, res)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (%+v)", got.Status, tc.want, got)
			}
		})
	}
}

func TestHealthCounts(t *testing.T) {
	h := newInfoHarness(t, topology(healthySet(6, 2), degradedSet(4, 2, 1), degradedSet(4, 2, 3)))
	got := decode[healthResponse](t, h.admin(http.MethodGet, "/liteio/admin/v1/health", nil))
	if got.Sets != 3 || got.Degraded != 1 || got.Unavailable != 1 {
		t.Errorf("counts = %+v, want sets=3 degraded=1 unavailable=1", got)
	}
	if got.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", got.Status)
	}
}

func TestInfoRequiresAuthorization(t *testing.T) {
	// The viewer has readonly (data plane), not admin:ServerInfo, so info is denied.
	h := newInfoHarness(t, topology(healthySet(4, 2)))
	if res := h.do(viewerCreds, http.MethodGet, "/liteio/admin/v1/info", nil); res.status != http.StatusForbidden {
		t.Errorf("viewer info status = %d, want 403", res.status)
	}
	if res := h.do(viewerCreds, http.MethodGet, "/liteio/admin/v1/health", nil); res.status != http.StatusForbidden {
		t.Errorf("viewer health status = %d, want 403", res.status)
	}
}

func TestInfoRoutesAbsentWithoutSource(t *testing.T) {
	// A server built without WithInfo does not register the routes: the admin gets a
	// 404, not a 500 from a nil source.
	h := newHarness(t)
	if res := h.admin(http.MethodGet, "/liteio/admin/v1/info", nil); res.status != http.StatusNotFound {
		t.Errorf("info without source status = %d, want 404", res.status)
	}
}

func BenchmarkServerInfo(b *testing.B) {
	src := fakeInfo{si: topology(healthySet(8, 4), healthySet(8, 4), degradedSet(8, 4, 1))}
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	server := NewServer(store, auth.NewStaticStore(adminCreds), WithInfo(src), WithVersion("dev"))
	req := httptest.NewRequest(http.MethodGet, "/liteio/admin/v1/info", nil)
	for b.Loop() {
		server.serverInfo(httptest.NewRecorder(), req)
	}
}
