package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

const taskExportLua = `
 if redis.call('EXISTS',KEYS[1])==0 then return {0} end
 if redis.call('STRLEN',KEYS[1])>tonumber(ARGV[2]) then return {2} end
 local raw=redis.call('GET',KEYS[1])
 local task=cjson.decode(raw)
 if task.id~=ARGV[1] or task.tenant_id~=ARGV[3] then return {0} end
 local count=0
 if type(task.trace)=='table' then count=#task.trace end
 if count>10000 then return {2} end
 local n=redis.call('LLEN',KEYS[2])
 if n~=0 and n~=count then return {3} end
 local result={1,raw}
 local total=#raw
 for _,value in ipairs(redis.call('LRANGE',KEYS[2],0,9999)) do
  if #value>40 then return {3} end
  total=total+#value
  if total>tonumber(ARGV[2]) then return {2} end
  result[#result+1]=value
 end
 if ARGV[4]~='joint' then return result end
`

var taskExportScript = redis.NewScript(taskExportLua)

func (r *RedisStore) ReadTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	if err := validateExportRequest(id, tenant, budget); err != nil {
		return nil, err
	}
	values, err := taskExportScript.RunRO(ctx, r.client, []string{r.taskKey(id), r.taskTraceTimeKey(id)}, id, budget, tenant).Slice()
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, ErrTaskExportUnavailable
	}
	switch values[0] {
	case int64(0):
		return nil, sql.ErrNoRows
	case int64(2):
		return nil, ErrTaskExportTooLarge
	case int64(3):
		return nil, ErrTaskExportUnavailable
	case int64(1):
	default:
		return nil, ErrTaskExportUnavailable
	}
	if len(values) < 2 {
		return nil, ErrTaskExportUnavailable
	}
	raw, ok := values[1].(string)
	if !ok {
		return nil, ErrTaskExportUnavailable
	}
	var task types.Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		return nil, err
	}
	if task.ID != id || task.TenantID != tenant || len(task.Trace) > MaxTaskExportTraces {
		return nil, ErrTaskExportUnavailable
	}
	if len(values) != 2 && len(values)-2 != len(task.Trace) {
		return nil, ErrTaskExportUnavailable
	}
	if !exportCopyFits(&task, budget) {
		return nil, ErrTaskExportTooLarge
	}
	events := make([]TaskTraceEvent, len(task.Trace))
	for i, tr := range task.Trace {
		events[i] = TaskTraceEvent{Sequence: int64(i + 1), Trace: tr}
		if len(values) > 2 {
			value, ok := values[i+2].(string)
			if !ok {
				return nil, ErrTaskExportUnavailable
			}
			if value != "" {
				at, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return nil, ErrTaskExportUnavailable
				}
				at = at.UTC()
				events[i].RecordedAt = &at
			}
		}
	}
	task.Trace = nil
	return &TaskExportSnapshot{Task: &task, Traces: events}, ctx.Err()
}
