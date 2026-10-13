package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"
)

// RunRO uses EVALSHA_RO/EVAL_RO. It never calls the mutating index migration.
// Old/unmigrated or oversized read models fail closed; callers may prepare
// indexes separately through their established operational workflow.
var retentionPageScript = redis.NewScript(`
 if redis.call('EXISTS',KEYS[1])==0 or redis.call('EXISTS',KEYS[2])==0 then return {0} end
 local members=redis.call('ZREVRANGEBYLEX',KEYS[3],ARGV[1],'-','LIMIT',0,ARGV[2])
 local output={1}
 for _,member in ipairs(members) do
  if #member>2048 then return {0} end
  local split=string.find(member,':',1,true)
  if not split then return {0} end
  local id=string.sub(member,split+1)
  local raw=redis.call('GET',ARGV[3]..id)
  if not raw or #raw>65536 then return {0} end
  local s=cjson.decode(raw)
  if s.id~=id or s.tenant_id~=ARGV[4] or s.index_member~=member then return {0} end
  local ttl=redis.call('PTTL',ARGV[5]..id)
  local lease=cjson.null
  if ttl==-2 then lease=false elseif ttl>=0 then lease=true end
  local row={terminal_since=s.terminal_since,last_maintenance_at=s.last_maintenance_at,maintenance_version=s.maintenance_version,task_id=s.id,tenant_id=s.tenant_id,session_id=s.session_id,status=s.status,created_at=s.created_at,updated_at=s.updated_at,lease_active=lease,trace_count=cjson.null,approvals_unresolved=cjson.null}
  output[#output+1]=cjson.encode(row)
 end
 return output
`)

func (r *RedisStore) ListTaskRetentionObservations(ctx context.Context, f TaskSummaryFilter) ([]RetentionObservation, error) {
	if err := validateRetentionFilter(f); err != nil {
		return nil, err
	}
	max := "+"
	if f.BeforeID != "" {
		max = "(" + taskSummaryMember(f.BeforeCreatedAt, f.BeforeID)
	}
	values, err := retentionPageScript.RunRO(ctx, r.client, []string{tasksIndexV2Marker, taskSummaryMigrationMarker, taskSummaryTenantBase + f.TenantID}, max, f.Limit, "task:summary:v1:", f.TenantID, "task:lease:").Slice()
	if err != nil {
		return nil, err
	}
	if len(values) == 0 || values[0] != int64(1) {
		return nil, ErrRetentionReadModelUnavailable
	}
	items := make([]RetentionObservation, 0, len(values)-1)
	for _, value := range values[1:] {
		raw, ok := value.(string)
		if !ok {
			return nil, errors.New("invalid retention metadata")
		}
		var item RetentionObservation
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}
