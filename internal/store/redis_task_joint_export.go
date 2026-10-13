package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Bound the complete scan rather than treating an incomplete legacy index as
// an authoritative list. No migrations, index repairs, leases or writes.
var taskJointExportScript = redis.NewScript(taskExportLua + `
 if redis.call('DBSIZE')>20000 then return {2} end
 local approvals={}
 local memories={}
 local seen={}
 local cursor='0'
 local rounds=0
 repeat
  local page=redis.call('SCAN',cursor,'COUNT',256)
  cursor=page[1];rounds=rounds+1
  if rounds>400 then return {2} end
  for _,key in ipairs(page[2]) do
   if not seen[key] and (string.sub(key,1,9)=='approval:' or string.sub(key,1,7)=='memory:') then
    seen[key]=true
    local size=redis.call('STRLEN',key)
    total=total+size
    if total>tonumber(ARGV[2]) then return {2} end
    local record=redis.call('GET',key)
    if record then
     local value=cjson.decode(record)
     local isApproval=string.sub(key,1,9)=='approval:'
     local taskID=isApproval and value.TaskID or value.task_id
     local tenantID=isApproval and value.TenantID or value.tenant_id
     local recordID=isApproval and value.ID or value.id
     local prefix=isApproval and 'approval:' or 'memory:'
     if type(recordID)~='string' or key~=prefix..recordID then return {3} end
     if type(taskID)~='string' or type(tenantID)~='string' then return {3} end
     if taskID==ARGV[1] then
      if tenantID~=ARGV[3] then return {3} end
      local target=isApproval and approvals or memories
      if #target>=1000 then return {2} end
      target[#target+1]=record
     end
    end
   end
  end
 until cursor=='0'
 local session=''
 local sequence='0'
 if task.session_id and task.session_id~='' then
  local sessionKey='session:'..task.session_id
  total=total+redis.call('STRLEN',sessionKey)
  if total>tonumber(ARGV[2]) then return {2} end
  session=redis.call('GET',sessionKey)
  if not session then return {3} end
  sequence=redis.call('GET','session:sequence:'..task.session_id) or '0'
  if #sequence>20 then return {3} end
 end
 return {1,raw,redis.call('LRANGE',KEYS[2],0,9999),approvals,memories,session,sequence}
`)

func (r *RedisStore) ReadJointTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	if err := validateExportRequest(id, tenant, budget); err != nil {
		return nil, err
	}
	values, err := taskJointExportScript.RunRO(ctx, r.client, []string{r.taskKey(id), r.taskTraceTimeKey(id)}, id, budget, tenant, "joint").Slice()
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
	if len(values) != 7 {
		return nil, ErrTaskExportUnavailable
	}
	raw, ok := values[1].(string)
	if !ok {
		return nil, ErrTaskExportUnavailable
	}
	task := &types.Task{}
	if err := json.Unmarshal([]byte(raw), task); err != nil {
		return nil, err
	}
	times, ok := values[2].([]any)
	if !ok {
		return nil, ErrTaskExportUnavailable
	}
	if len(times) != 0 && len(times) != len(task.Trace) {
		return nil, ErrTaskExportUnavailable
	}
	events := make([]TaskTraceEvent, len(task.Trace))
	for i, tr := range task.Trace {
		events[i] = TaskTraceEvent{Sequence: int64(i + 1), Trace: tr}
		if len(times) > 0 {
			value, ok := times[i].(string)
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
	d := newExportDependencies()
	approvals, ok := values[3].([]any)
	if !ok {
		return nil, ErrTaskExportUnavailable
	}
	for _, raw := range approvals {
		value, ok := raw.(string)
		if !ok {
			return nil, ErrTaskExportUnavailable
		}
		a := &types.DurableApproval{}
		if err := decodeExportApproval([]byte(value), a); err != nil {
			return nil, err
		}
		d.Approvals = append(d.Approvals, a)
	}
	memories, ok := values[4].([]any)
	if !ok {
		return nil, ErrTaskExportUnavailable
	}
	for _, raw := range memories {
		value, ok := raw.(string)
		if !ok {
			return nil, ErrTaskExportUnavailable
		}
		mem := &types.Memory{}
		if err := json.Unmarshal([]byte(value), mem); err != nil {
			return nil, err
		}
		d.Memories = append(d.Memories, mem)
	}
	session, ok := values[5].(string)
	if !ok {
		return nil, ErrTaskExportUnavailable
	}
	if session != "" {
		s := &types.Session{}
		if err := json.Unmarshal([]byte(session), s); err != nil {
			return nil, err
		}
		value, ok := values[6].(string)
		if !ok {
			return nil, ErrTaskExportUnavailable
		}
		seq, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, ErrTaskExportUnavailable
		}
		s.NextSequence = seq
		d.Session = &TaskExportSession{Session: s, NextSequence: seq}
	}
	if err := validateExportDependencies(task, d); err != nil {
		return nil, err
	}
	sortExportDependencies(d)
	result := &TaskExportSnapshot{Task: task, Traces: events, Dependencies: d}
	if !exportCopyFits(result, budget) {
		return nil, ErrTaskExportTooLarge
	}
	return result, ctx.Err()
}
