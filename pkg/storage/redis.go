package storage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/extra/redisotel/v9"
	goredislib "github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

func init() {
	if err := RegisterStorage(new(RedisDB)); err != nil {
		panic(err)
	}
}

const (
	NamespaceKeySeparator           = ":"
	Pong                            = "PONG"
	RedisScanBatchSize              = 1000
	MaxElapsedTime                  = 6 * time.Second
	RedisAddressOption    OptionKey = "redis-address-option"
)

type RedisDB struct {
	db *goredislib.Client
}

func (b *RedisDB) ReadPage(ctx context.Context, namespace string, pageToken string, pageSize int) (map[string][]byte, string, error) {
	cursor := uint64(0)
	if pageToken != "" {
		var err error
		cursor, err = strconv.ParseUint(pageToken, 10, 64)
		if err != nil {
			return nil, "", errors.Wrap(err, "parsing page token")
		}
	}

	keys, nextCursor, err := readAllKeys(ctx, namespace, b, pageSize, cursor)
	if err != nil {
		return nil, "", err
	}
	results, err := readAll(ctx, keys, b)
	if err != nil {
		return nil, "", err
	}
	return results, nextCursor, nil
}

var _ ServiceStorage = (*RedisDB)(nil)

type redisTx struct {
	pipe goredislib.Pipeliner
}

func (rtx *redisTx) Write(ctx context.Context, namespace, key string, value []byte) error {
	nameSpaceKey := getRedisKey(namespace, key)
	return rtx.pipe.Set(ctx, nameSpaceKey, value, 0).Err()
}

func (b *RedisDB) Init(opts ...Option) error {
	address, password, err := processRedisOptions(opts...)
	if err != nil {
		return errors.Wrap(err, "processing redis options")
	}
	client := goredislib.NewClient(&goredislib.Options{
		Addr:     address,
		Password: password,
	})

	if err = redisotel.InstrumentTracing(client); err != nil {
		return errors.Wrap(err, "instrumenting tracing")
	}

	if err = redisotel.InstrumentMetrics(client); err != nil {
		return errors.Wrap(err, "instrumenting metrics")
	}

	b.db = client

	return nil
}

func processRedisOptions(opts ...Option) (address, password string, err error) {
	if len(opts) != 2 {
		return "", "", errors.New("redis options must contain address and password")
	}
	for _, opt := range opts {
		switch opt.ID {
		case RedisAddressOption:
			maybeAddress, ok := opt.Option.(string)
			if !ok {
				err = errors.New("redis address must be a string")
				return
			}
			if len(maybeAddress) == 0 {
				err = errors.New("redis address must not be empty")
				return
			}
			address = maybeAddress
		case PasswordOption:
			maybePassword, ok := opt.Option.(string)
			if !ok {
				err = errors.New("redis password must be a string")
				return
			}
			if len(maybePassword) == 0 {
				err = errors.New("redis password must not be empty")
				return
			}
			password = maybePassword
		}
	}
	if len(address) == 0 || len(password) == 0 {
		err = errors.New("redis address and password must not be empty")
		return
	}
	return address, password, nil
}

func (b *RedisDB) URI() string {
	return b.db.Options().Addr
}

func (b *RedisDB) IsOpen() bool {
	pong, err := b.db.Ping(context.Background()).Result()
	if err != nil {
		logrus.Error(err)
		return false
	}

	return pong == Pong
}

func (b *RedisDB) Type() Type {
	return Redis
}

func (b *RedisDB) Close() error {
	return b.db.Close()
}

func (b *RedisDB) Execute(ctx context.Context, businessLogicFunc BusinessLogicFunc, watchKeys []WatchKey) (any, error) {
	var finalOutput any
	// Transactional function.
	txf := func(tx *goredislib.Tx) error {
		// Operation is commited only if the watched keys remain unchanged.
		_, err := tx.TxPipelined(ctx, func(pipe goredislib.Pipeliner) error {
			redisTx := redisTx{pipe}
			var err error

			finalOutput, err = businessLogicFunc(ctx, &redisTx)
			if err != nil {
				return err
			}
			return nil
		})
		return err
	}

	watchKeysStr := make([]string, 0)

	for _, wc := range watchKeys {
		watchKeysStr = append(watchKeysStr, getRedisKey(wc.Namespace, wc.Key))
	}

	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.MaxElapsedTime = MaxElapsedTime

	err := backoff.Retry(func() error {
		err := b.db.Watch(ctx, txf, watchKeysStr...)
		if err != nil && errors.Is(err, goredislib.TxFailedErr) {
			logrus.Warn("Optimistic lock lost. Retrying..")
			return err
		}
		return backoff.Permanent(err)
	}, expBackoff)

	if err != nil {
		logrus.Errorf("error after retrying: %v", err)
		return nil, errors.Wrap(err, "failed to execute after retrying")
	}

	return finalOutput, nil
}

func (b *RedisDB) Exists(ctx context.Context, namespace, key string) (bool, error) {
	nameSpaceKey := getRedisKey(namespace, key)
	existsInt, err := b.db.Exists(ctx, nameSpaceKey).Result()
	if err != nil {
		return false, errors.Wrap(err, "checking if exists")
	}

	exists := existsInt != 0
	return exists, nil
}

func (b *RedisDB) Write(ctx context.Context, namespace, key string, value []byte) error {
	nameSpaceKey := getRedisKey(namespace, key)
	return b.db.Set(ctx, nameSpaceKey, value, 0).Err()
}

func (b *RedisDB) WriteMany(ctx context.Context, namespaces, keys []string, values [][]byte) error {
	if len(namespaces) != len(keys) && len(namespaces) != len(values) {
		return errors.New("namespaces, keys, and values, are not of equal length")
	}

	valuesToSet := make([]string, 0, 2*len(values))
	for i := range namespaces {
		valuesToSet = append(valuesToSet, getRedisKey(namespaces[i], keys[i]))
		valuesToSet = append(valuesToSet, string(values[i]))
	}

	return b.db.MSet(ctx, valuesToSet).Err()
}

func (b *RedisDB) Read(ctx context.Context, namespace, key string) ([]byte, error) {
	nameSpaceKey := getRedisKey(namespace, key)

	res, err := b.db.Get(ctx, nameSpaceKey).Bytes()

	// Nil reply returned by Redis when key does not exist.
	if errors.Is(err, goredislib.Nil) {
		return res, nil
	}

	return res, err
}

func (b *RedisDB) ReadPrefix(ctx context.Context, namespace, prefix string) (map[string][]byte, error) {
	namespacePrefix := getRedisKey(namespace, prefix)

	keys, _, err := readAllKeys(ctx, namespacePrefix, b, -1, 0)
	if err != nil {
		return nil, errors.Wrap(err, "read all keys")
	}

	return readAll(ctx, keys, b)
}

func (b *RedisDB) ReadAll(ctx context.Context, namespace string) (map[string][]byte, error) {
	keys, _, err := readAllKeys(ctx, namespace, b, -1, 0)
	if err != nil {
		return nil, errors.Wrap(err, "read all keys")
	}

	return readAll(ctx, keys, b)
}

// TODO: This potentially could dangerous as it might run out of memory as we populate result
func readAll(ctx context.Context, keys []string, b *RedisDB) (map[string][]byte, error) {
	result := make(map[string][]byte, len(keys))

	if len(keys) == 0 {
		return nil, nil
	}

	values, err := b.db.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, errors.Wrap(err, "getting multiple keys")
	}

	if len(keys) != len(values) {
		return nil, errors.New("key length does not match value length")
	}

	// result needs to take the namespace out of the key
	namespaceDashIndex := strings.Index(keys[0], NamespaceKeySeparator)
	for i, val := range values {
		byteValue := []byte(fmt.Sprintf("%v", val))
		key := keys[i][namespaceDashIndex+1:]
		result[key] = byteValue
	}

	return result, nil
}

func (b *RedisDB) ReadAllKeys(ctx context.Context, namespace string) ([]string, error) {
	keys, _, err := readAllKeys(ctx, namespace, b, -1, 0)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return make([]string, 0), nil
	}

	namespaceDashIndex := strings.Index(keys[0], NamespaceKeySeparator)
	for i, key := range keys {
		keyWithoutNamespace := key[namespaceDashIndex+1:]
		keys[i] = keyWithoutNamespace
	}

	return keys, nil
}

// NOTE: When passing pageSize == -1, **all** items are returns. Exercise caution regarding memory limits. Always
// prefer to set the pageSize.
// TODO: Remove all calls that set pageSize to -1. https://github.com/TBD54566975/ssi-service/issues/525
func readAllKeys(ctx context.Context, namespace string, b *RedisDB, pageSize int, cursor uint64) ([]string, string, error) {

	var allKeys []string

	var nextCursor uint64
	var err error
	var keys []string
	scanCount := RedisScanBatchSize
	if pageSize != -1 {
		scanCount = min(RedisScanBatchSize, pageSize)
	}
	for pageSize == -1 || (len(allKeys) < pageSize) {
		keys, nextCursor, err = b.db.Scan(ctx, cursor, namespace+"*", int64(scanCount)).Result()
		if err != nil {
			return nil, "", errors.Wrap(err, "scan error")
		}

		allKeys = append(allKeys, keys...)

		if nextCursor == 0 {
			break
		}

		cursor = nextCursor
	}

	var nextCursorToReturn string
	if nextCursor != 0 {
		nextCursorToReturn = strconv.FormatUint(nextCursor, 10)
	}
	return allKeys, nextCursorToReturn, nil
}

func min(l int, r int) int {
	if l <= r {
		return l
	}
	return r
}

func (b *RedisDB) Delete(ctx context.Context, namespace, key string) error {
	nameSpaceKey := getRedisKey(namespace, key)

	if !namespaceExists(ctx, namespace, b) {
		return errors.Errorf("namespace<%s> does not exist", namespace)
	}

	res, err := b.db.GetDel(ctx, nameSpaceKey).Result()
	if res == "" {
		return errors.Wrapf(err, "key<%s> and namespace<%s> does not exist", key, namespace)
	}

	return err

}

func (b *RedisDB) DeleteNamespace(ctx context.Context, namespace string) error {
	keys, _, err := readAllKeys(ctx, namespace, b, -1, 0)
	if err != nil {
		return errors.Wrap(err, "read all keys")
	}

	if len(keys) == 0 {
		return errors.Errorf("could not delete namespace<%s>, namespace does not exist", namespace)
	}

	return b.db.Del(ctx, keys...).Err()
}

func (b *RedisDB) Update(ctx context.Context, namespace string, key string, values map[string]any) ([]byte, error) {
	updatedData, err := txWithUpdater(ctx, namespace, key, NewUpdater(values), b)
	return updatedData, err
}

func (b *RedisDB) UpdateValueAndOperation(ctx context.Context, namespace, key string, updater Updater, opNamespace, opKey string, opUpdater ResponseSettingUpdater) (first, op []byte, err error) {
	// The Pipeliner interface provided by the go-redis library guarantees that all the commands queued in the pipeline will either succeed or fail together.
	_, err = b.db.TxPipelined(ctx, func(pipe goredislib.Pipeliner) error {

		firstTx, err := txWithUpdater(ctx, namespace, key, updater, b)
		if err != nil {
			return err
		}

		opUpdater.SetUpdatedResponse(firstTx)
		secondTx, err := txWithUpdater(ctx, opNamespace, opKey, opUpdater, b)
		if err != nil {
			return err
		}

		first = firstTx
		op = secondTx

		return nil
	})

	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to execute transaction")
	}

	return first, op, err
}

func txWithUpdater(ctx context.Context, namespace, key string, updater Updater, b *RedisDB) ([]byte, error) {
	nameSpaceKey := getRedisKey(namespace, key)
	v, err := b.db.Get(ctx, nameSpaceKey).Bytes()
	if err != nil {
		return nil, errors.Wrapf(err, "get error with namespace: %s key: %s", namespace, key)
	}
	if v == nil {
		return nil, errors.Errorf("key not found %s", key)
	}
	if err := updater.Validate(v); err != nil {
		return nil, errors.Wrapf(err, "validating update")
	}

	data, err := updater.Update(v)
	if err != nil {
		return nil, err
	}

	if err = b.db.Set(ctx, nameSpaceKey, data, 0).Err(); err != nil {
		return nil, errors.Wrap(err, "writing to db")
	}

	return data, nil
}

func getRedisKey(namespace, key string) string {
	return namespace + NamespaceKeySeparator + key
}

func namespaceExists(ctx context.Context, namespace string, b *RedisDB) bool {
	keys, _ := b.db.Scan(ctx, 0, namespace+"*", RedisScanBatchSize).Val()

	if len(keys) == 0 {
		return false
	}

	return true
}
