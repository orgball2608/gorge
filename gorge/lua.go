package gorge

// --- LUA SCRIPTS ---

// tryLockAndGetScript: Try to get a value. If not present, try to acquire a distributed lock.
// KEYS[1]: key
// ARGV[1]: ownerID of the current instance
// ARGV[2]: lock TTL (in seconds) // FIX: Unit is seconds
// Returns: { value, status }
// status can be: "HIT", "LOCKED_BY_OTHER", "ACQUIRED_LOCK"
const tryLockAndGetScript = `
local val = redis.call('HGET', KEYS[1], 'value')
if val then
    return {val, 'HIT'}
end
local lockOwner = redis.call('HGET', KEYS[1], 'lockOwner')
if lockOwner and lockOwner ~= ARGV[1] then
    return {nil, 'LOCKED_BY_OTHER'}
end
redis.call('HSET', KEYS[1], 'lockOwner', ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return {nil, 'ACQUIRED_LOCK'}
`

// setDataAndUnlockScript: Set a new value and release the lock.
// KEYS[1]: key
// ARGV[1]: ownerID
// ARGV[2]: serialized value
// ARGV[3]: TTL of the key (in seconds)
// Returns: 1 if successful, 0 if not the lock owner
const setDataAndUnlockScript = `
if redis.call('HGET', KEYS[1], 'lockOwner') == ARGV[1] then
    redis.call('HSET', KEYS[1], 'value', ARGV[2])
    redis.call('HDEL', KEYS[1], 'lockOwner')
    redis.call('EXPIRE', KEYS[1], ARGV[3])
    return 1
end
return 0
`
