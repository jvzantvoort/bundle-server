package server

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jvzantvoort/bundle-server/internal/config"
	bundlelib "github.com/jvzantvoort/bundle/bundle"
)

type cacheEntry struct {
	bundles     []bundleRecord
	refreshedAt time.Time
}

type metadataCache struct {
	mu    sync.RWMutex
	pools map[string]cacheEntry
}

func (c *metadataCache) get(pool string) ([]bundleRecord, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.pools[pool]
	if !ok {
		return nil, false
	}
	return append([]bundleRecord(nil), entry.bundles...), true
}

func (c *metadataCache) set(pool string, bundles []bundleRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pools[pool] = cacheEntry{bundles: append([]bundleRecord(nil), bundles...), refreshedAt: time.Now().UTC()}
}

func (c *metadataCache) remove(pool string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pools, pool)
}

func (a *App) enqueueScheduledAction(ctx context.Context, schedule config.ScheduleConfig) {
	_, _ = a.queue.Enqueue(ctx, "scheduled:"+schedule.Action, func(context.Context) (any, error) {
		pools := a.schedulePools(schedule)
		results := make(map[string]any, len(pools))
		for _, pool := range pools {
			result, err := a.runScheduledAction(pool, schedule.Action)
			if err != nil {
				return nil, fmt.Errorf("%s for pool %s: %w", schedule.Action, pool, err)
			}
			results[pool] = result
		}
		return map[string]any{"schedule": schedule.Name, "action": schedule.Action, "pools": results}, nil
	})
}

func (a *App) schedulePools(schedule config.ScheduleConfig) []string {
	if len(schedule.Pools) > 0 {
		return append([]string(nil), schedule.Pools...)
	}
	pools := make([]string, 0, len(a.config.Pools))
	for pool := range a.config.Pools {
		pools = append(pools, pool)
	}
	sort.Strings(pools)
	return pools
}

func (a *App) runScheduledAction(pool, action string) (any, error) {
	switch action {
	case "verify":
		records, err := a.scanBundles(pool)
		if err != nil {
			return nil, err
		}
		verified, failed := 0, 0
		for _, record := range records {
			ok, _, err := bundlelib.Verify(record.Path)
			if err != nil {
				return nil, err
			}
			if ok {
				verified++
			} else {
				failed++
			}
		}
		a.invalidateMetadata(pool)
		return map[string]int{"verified_bundles": verified, "failed_bundles": failed}, nil
	case "collect_status":
		records, err := a.scanBundles(pool)
		if err != nil {
			return nil, err
		}
		verified, failed, unchecked := 0, 0, 0
		for _, record := range records {
			if record.State.LastChecked.IsZero() {
				unchecked++
			} else if record.State.Verified {
				verified++
			} else {
				failed++
			}
		}
		return map[string]int{"bundles": len(records), "verified_bundles": verified, "failed_bundles": failed, "unchecked_bundles": unchecked}, nil
	case "cache_metadata":
		count, err := a.refreshMetadata(pool)
		if err != nil {
			return nil, err
		}
		return map[string]int{"cached_bundles": count}, nil
	default:
		return nil, fmt.Errorf("unsupported scheduled action %q", action)
	}
}
