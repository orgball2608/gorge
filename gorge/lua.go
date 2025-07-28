package gorge

const (
	// tryLockAndGetScript tries to get a key. If the key does not exist, it tries to
	// acquire a lock.
	//
	// KEYS[1]: The key to get or lock.
	// ARGV[1]: The lock owner ID.
	// ARGV[2]: The lock TTL in seconds.
	//
	// Returns:
	// { value, "HIT" } if the key exists.
	// { "", "ACQUIRED_LOCK" } if the lock was acquired.
	// { "", "LOCKED_BY_OTHER" } if the key is locked by another owner.
	tryLockAndGetScript = `
local lockOwner = redis.call('HGET', KEYS[1], 'lockOwner')
if lockOwner then
    if lockOwner == ARGV[1] then
        -- We already own the lock, just extend it
        redis.call('PEXPIRE', KEYS[1], ARGV[2] * 1000)
        return {'', 'ACQUIRED_LOCK'}
    else
        return {'', 'LOCKED_BY_OTHER'}
    end
end

-- No lock exists, check for data
local value = redis.call('HGET', KEYS[1], 'value')
if value then
    return {value, 'HIT'}
end

-- No lock and no data, acquire lock
redis.call('HSET', KEYS[1], 'lockOwner', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2] * 1000)
return {'', 'ACQUIRED_LOCK'}
`

	// setDataAndUnlockScript sets the value for a key and releases the lock if
	// this instance owns it.
	//
	// KEYS[1]: The key to set.
	// ARGV[1]: The lock owner ID to check against.
	// ARGV[2]: The value to set.
	// ARGV[3]: The TTL for the key in seconds.
	//
	// Returns:
	// 1 if the operation was successful.
	// 0 if the lock was owned by someone else.
	setDataAndUnlockScript = `
if redis.call('HGET', KEYS[1], 'lockOwner') == ARGV[1] then
    redis.call('HSET', KEYS[1], 'value', ARGV[2])
    redis.call('HDEL', KEYS[1], 'lockOwner')
    redis.call('EXPIRE', KEYS[1], ARGV[3])
    return 1
end
return 0
`
)
