package store

import (
	"github.com/Dataman-Cloud/swan/src/manager/raft/types"

	"github.com/boltdb/bolt"
)

func withCreateSlotBucketIfNotExists(tx *bolt.Tx, appId, slotId string, fn func(bkt *bolt.Bucket) error) error {
	bkt, err := createBucketIfNotExists(tx, bucketKeyStorageVersion, bucketKeyApps, []byte(appId), bucketKeySlots, []byte(slotId))
	if err != nil {
		return err
	}

	return fn(bkt)
}

func WithSlotBucket(tx *bolt.Tx, appId, slotId string, fn func(bkt *bolt.Bucket) error) error {
	bkt := GetSlotBucket(tx, appId, slotId)
	if bkt == nil {
		return ErrSlotUnknown
	}

	return fn(bkt)
}

func GetSlotsBucket(tx *bolt.Tx, appId string) *bolt.Bucket {
	return getBucket(tx, bucketKeyStorageVersion, bucketKeyApps, []byte(appId), bucketKeySlots)
}

func GetSlotBucket(tx *bolt.Tx, appId, slotId string) *bolt.Bucket {
	return getBucket(tx, bucketKeyStorageVersion, bucketKeyApps, []byte(appId), bucketKeySlots, []byte(slotId))
}

func putSlot(tx *bolt.Tx, slot *types.Slot) error {
	return withCreateSlotBucketIfNotExists(tx, slot.AppId, slot.Id, func(bkt *bolt.Bucket) error {
		p, err := slot.Marshal()
		if err != nil {
			return err
		}

		return bkt.Put(BucketKeyData, p)
	})
}

func removeSlot(tx *bolt.Tx, appId, slotId string) error {
	slotsBkt := GetSlotsBucket(tx, appId)

	if slotsBkt == nil {
		return nil
	}

	return slotsBkt.DeleteBucket([]byte(slotId))
}
