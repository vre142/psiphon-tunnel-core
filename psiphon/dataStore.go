/*
 * Copyright (c) 2015, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Psiphon-Labs/bolt"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/parameters"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

const (
	serverEntriesBucket         = "serverEntries"
	rankedServerEntriesBucket   = "rankedServerEntries"
	rankedServerEntriesKey      = "rankedServerEntries"
	splitTunnelRouteETagsBucket = "splitTunnelRouteETags"
	splitTunnelRouteDataBucket  = "splitTunnelRouteData"
	urlETagsBucket              = "urlETags"
	keyValueBucket              = "keyValues"
	tunnelStatsBucket           = "tunnelStats"
	remoteServerListStatsBucket = "remoteServerListStats"
	slokBucket                  = "SLOKs"
	tacticsBucket               = "tactics"
	speedTestSamplesBucket      = "speedTestSamples"

	rankedServerEntryCount = 100
)

const (
	DATA_STORE_FILENAME                     = "psiphon.boltdb"
	DATA_STORE_LAST_CONNECTED_KEY           = "lastConnected"
	DATA_STORE_LAST_SERVER_ENTRY_FILTER_KEY = "lastServerEntryFilter"
	PERSISTENT_STAT_TYPE_REMOTE_SERVER_LIST = remoteServerListStatsBucket
)

var (
	datastoreInitalizeMutex sync.Mutex
	datastoreReferenceMutex sync.Mutex
	datastoreDB             *bolt.DB
)

// OpenDataStore opens and initializes the singleton data store instance.
func OpenDataStore(config *Config) error {

	datastoreInitalizeMutex.Lock()
	defer datastoreInitalizeMutex.Unlock()

	datastoreReferenceMutex.Lock()
	existingDB := datastoreDB
	datastoreReferenceMutex.Unlock()

	if existingDB != nil {
		return common.ContextError(errors.New("db already open"))
	}

	filename := filepath.Join(config.DataStoreDirectory, DATA_STORE_FILENAME)

	var newDB *bolt.DB
	var err error

	for retry := 0; retry < 3; retry++ {

		if retry > 0 {
			NoticeAlert("OpenDataStore retry: %d", retry)
		}

		newDB, err = bolt.Open(filename, 0600, &bolt.Options{Timeout: 1 * time.Second})

		// The datastore file may be corrupt, so attempt to delete and try again
		if err != nil {
			NoticeAlert("bolt.Open error: %s", err)
			os.Remove(filename)
			continue
		}

		// Run consistency checks on datastore and emit errors for diagnostics purposes
		// We assume this will complete quickly for typical size Psiphon datastores.
		err = newDB.View(func(tx *bolt.Tx) error {
			return tx.SynchronousCheck()
		})

		// The datastore file may be corrupt, so attempt to delete and try again
		if err != nil {
			NoticeAlert("bolt.SynchronousCheck error: %s", err)
			newDB.Close()
			os.Remove(filename)
			continue
		}

		break
	}

	if err != nil {
		return common.ContextError(fmt.Errorf("failed to open database: %s", err))
	}

	err = newDB.Update(func(tx *bolt.Tx) error {
		requiredBuckets := []string{
			serverEntriesBucket,
			rankedServerEntriesBucket,
			splitTunnelRouteETagsBucket,
			splitTunnelRouteDataBucket,
			urlETagsBucket,
			keyValueBucket,
			tunnelStatsBucket,
			remoteServerListStatsBucket,
			slokBucket,
			tacticsBucket,
			speedTestSamplesBucket,
		}
		for _, bucket := range requiredBuckets {
			_, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return common.ContextError(fmt.Errorf("failed to create buckets: %s", err))
	}

	// Cleanup obsolete tunnel (session) stats bucket, if one still exists

	err = newDB.Update(func(tx *bolt.Tx) error {
		tunnelStatsBucket := []byte("tunnelStats")
		if tx.Bucket(tunnelStatsBucket) != nil {
			err := tx.DeleteBucket(tunnelStatsBucket)
			if err != nil {
				NoticeAlert("DeleteBucket %s error: %s", tunnelStatsBucket, err)
				// Continue, since this is not fatal
			}
		}
		return nil
	})
	if err != nil {
		return common.ContextError(fmt.Errorf("failed to create buckets: %s", err))
	}

	datastoreReferenceMutex.Lock()
	datastoreDB = newDB
	datastoreReferenceMutex.Unlock()

	_ = resetAllPersistentStatsToUnreported()

	return nil
}

// CloseDataStore closes the singleton data store instance, if open.
func CloseDataStore() {

	datastoreInitalizeMutex.Lock()
	defer datastoreInitalizeMutex.Unlock()

	datastoreReferenceMutex.Lock()
	defer datastoreReferenceMutex.Unlock()

	if datastoreDB == nil {
		return
	}

	err := datastoreDB.Close()
	if err != nil {
		NoticeAlert("failed to close database: %s", err)
	}

	datastoreDB = nil
}

func dataStoreView(fn func(tx *bolt.Tx) error) error {

	datastoreReferenceMutex.Lock()
	db := datastoreDB
	datastoreReferenceMutex.Unlock()

	if db == nil {
		return common.ContextError(errors.New("database not open"))
	}

	return db.View(fn)
}

func dataStoreUpdate(fn func(tx *bolt.Tx) error) error {

	datastoreReferenceMutex.Lock()
	db := datastoreDB
	datastoreReferenceMutex.Unlock()

	if db == nil {
		return common.ContextError(errors.New("database not open"))
	}

	return db.Update(fn)
}

// StoreServerEntry adds the server entry to the data store.
// A newly stored (or re-stored) server entry is assigned the next-to-top
// rank for iteration order (the previous top ranked entry is promoted). The
// purpose of inserting at next-to-top is to keep the last selected server
// as the top ranked server.
//
// When a server entry already exists for a given server, it will be
// replaced only if replaceIfExists is set or if the the ConfigurationVersion
// field of the new entry is strictly higher than the existing entry.
//
// If the server entry data is malformed, an alert notice is issued and
// the entry is skipped; no error is returned.
func StoreServerEntry(serverEntryFields protocol.ServerEntryFields, replaceIfExists bool) error {

	// Server entries should already be validated before this point,
	// so instead of skipping we fail with an error.
	err := protocol.ValidateServerEntryFields(serverEntryFields)
	if err != nil {
		return common.ContextError(
			fmt.Errorf("invalid server entry: %s", err))
	}

	// BoltDB implementation note:
	// For simplicity, we don't maintain indexes on server entry
	// region or supported protocols. Instead, we perform full-bucket
	// scans with a filter. With a small enough database (thousands or
	// even tens of thousand of server entries) and common enough
	// values (e.g., many servers support all protocols), performance
	// is expected to be acceptable.

	err = dataStoreUpdate(func(tx *bolt.Tx) error {

		serverEntries := tx.Bucket([]byte(serverEntriesBucket))

		ipAddress := serverEntryFields.GetIPAddress()

		// Check not only that the entry exists, but is valid. This
		// will replace in the rare case where the data is corrupt.
		existingConfigurationVersion := -1
		existingData := serverEntries.Get([]byte(ipAddress))
		if existingData != nil {
			var existingServerEntry *protocol.ServerEntry
			err := json.Unmarshal(existingData, &existingServerEntry)
			if err == nil {
				existingConfigurationVersion = existingServerEntry.ConfigurationVersion
			}
		}

		exists := existingConfigurationVersion > -1
		newer := exists && existingConfigurationVersion < serverEntryFields.GetConfigurationVersion()
		update := !exists || replaceIfExists || newer

		if !update {
			// Disabling this notice, for now, as it generates too much noise
			// in diagnostics with clients that always submit embedded servers
			// to the core on each run.
			// NoticeInfo("ignored update for server %s", serverEntry.IpAddress)
			return nil
		}

		data, err := json.Marshal(serverEntryFields)
		if err != nil {
			return common.ContextError(err)
		}
		err = serverEntries.Put([]byte(ipAddress), data)
		if err != nil {
			return common.ContextError(err)
		}

		err = insertRankedServerEntry(tx, ipAddress, 1)
		if err != nil {
			return common.ContextError(err)
		}

		NoticeInfo("updated server %s", ipAddress)

		return nil
	})
	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// StoreServerEntries stores a list of server entries.
// There is an independent transaction for each entry insert/update.
func StoreServerEntries(
	config *Config,
	serverEntries []protocol.ServerEntryFields,
	replaceIfExists bool) error {

	for _, serverEntryFields := range serverEntries {
		err := StoreServerEntry(serverEntryFields, replaceIfExists)
		if err != nil {
			return common.ContextError(err)
		}
	}

	return nil
}

// StreamingStoreServerEntries stores a list of server entries.
// There is an independent transaction for each entry insert/update.
func StreamingStoreServerEntries(
	config *Config,
	serverEntries *protocol.StreamingServerEntryDecoder,
	replaceIfExists bool) error {

	// Note: both StreamingServerEntryDecoder.Next and StoreServerEntry
	// allocate temporary memory buffers for hex/JSON decoding/encoding,
	// so this isn't true constant-memory streaming (it depends on garbage
	// collection).

	for {
		serverEntry, err := serverEntries.Next()
		if err != nil {
			return common.ContextError(err)
		}

		if serverEntry == nil {
			// No more server entries
			break
		}

		err = StoreServerEntry(serverEntry, replaceIfExists)
		if err != nil {
			return common.ContextError(err)
		}
	}

	return nil
}

// PromoteServerEntry assigns the top rank (one more than current
// max rank) to the specified server entry. Server candidates are
// iterated in decending rank order, so this server entry will be
// the first candidate in a subsequent tunnel establishment.
func PromoteServerEntry(config *Config, ipAddress string) error {
	err := dataStoreUpdate(func(tx *bolt.Tx) error {

		// Ensure the corresponding entry exists before
		// inserting into rank.
		bucket := tx.Bucket([]byte(serverEntriesBucket))
		data := bucket.Get([]byte(ipAddress))
		if data == nil {
			NoticeAlert(
				"PromoteServerEntry: ignoring unknown server entry: %s",
				ipAddress)
			return nil
		}

		err := insertRankedServerEntry(tx, ipAddress, 0)
		if err != nil {
			return err
		}

		// Store the current server entry filter (e.g, region, etc.) that
		// was in use when the entry was promoted. This is used to detect
		// when the top ranked server entry was promoted under a different
		// filter.

		currentFilter, err := makeServerEntryFilterValue(config)
		if err != nil {
			return err
		}
		bucket = tx.Bucket([]byte(keyValueBucket))
		return bucket.Put([]byte(DATA_STORE_LAST_SERVER_ENTRY_FILTER_KEY), currentFilter)
	})

	if err != nil {
		return common.ContextError(err)
	}
	return nil
}

func makeServerEntryFilterValue(config *Config) ([]byte, error) {

	// Currently, only a change of EgressRegion will "break" server affinity.
	// If the tunnel protocol filter changes, any existing affinity server
	// either passes the new filter, or it will be skipped anyway.

	return []byte(config.EgressRegion), nil
}

func hasServerEntryFilterChanged(config *Config) (bool, error) {

	currentFilter, err := makeServerEntryFilterValue(config)
	if err != nil {
		return false, common.ContextError(err)
	}

	changed := false
	err = dataStoreView(func(tx *bolt.Tx) error {

		// previousFilter will be nil not found (not previously
		// set) which will never match any current filter.

		bucket := tx.Bucket([]byte(keyValueBucket))
		previousFilter := bucket.Get([]byte(DATA_STORE_LAST_SERVER_ENTRY_FILTER_KEY))
		if bytes.Compare(previousFilter, currentFilter) != 0 {
			changed = true
		}
		return nil
	})
	if err != nil {
		return false, common.ContextError(err)
	}

	return changed, nil
}

func getRankedServerEntries(tx *bolt.Tx) ([]string, error) {
	bucket := tx.Bucket([]byte(rankedServerEntriesBucket))
	data := bucket.Get([]byte(rankedServerEntriesKey))

	if data == nil {
		return []string{}, nil
	}

	rankedServerEntries := make([]string, 0)
	err := json.Unmarshal(data, &rankedServerEntries)
	if err != nil {
		return nil, common.ContextError(err)
	}
	return rankedServerEntries, nil
}

func setRankedServerEntries(tx *bolt.Tx, rankedServerEntries []string) error {
	data, err := json.Marshal(rankedServerEntries)
	if err != nil {
		return common.ContextError(err)
	}

	bucket := tx.Bucket([]byte(rankedServerEntriesBucket))
	err = bucket.Put([]byte(rankedServerEntriesKey), data)
	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

func insertRankedServerEntry(tx *bolt.Tx, serverEntryId string, position int) error {
	rankedServerEntries, err := getRankedServerEntries(tx)
	if err != nil {
		return common.ContextError(err)
	}

	// BoltDB implementation note:
	// For simplicity, we store the ranked server ids in an array serialized to
	// a single key value. To ensure this value doesn't grow without bound,
	// it's capped at rankedServerEntryCount. For now, this cap should be large
	// enough to meet the shuffleHeadLength = config.TunnelPoolSize criteria, for
	// any reasonable configuration of config.TunnelPoolSize.

	// Using: https://github.com/golang/go/wiki/SliceTricks

	// When serverEntryId is already ranked, remove it first to avoid duplicates

	for i, rankedServerEntryId := range rankedServerEntries {
		if rankedServerEntryId == serverEntryId {
			rankedServerEntries = append(
				rankedServerEntries[:i], rankedServerEntries[i+1:]...)
			break
		}
	}

	// SliceTricks insert, with length cap enforced

	if len(rankedServerEntries) < rankedServerEntryCount {
		rankedServerEntries = append(rankedServerEntries, "")
	}
	if position >= len(rankedServerEntries) {
		position = len(rankedServerEntries) - 1
	}
	copy(rankedServerEntries[position+1:], rankedServerEntries[position:])
	rankedServerEntries[position] = serverEntryId

	err = setRankedServerEntries(tx, rankedServerEntries)
	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// ServerEntryIterator is used to iterate over
// stored server entries in rank order.
type ServerEntryIterator struct {
	config                       *Config
	shuffleHeadLength            int
	serverEntryIds               []string
	serverEntryIndex             int
	isTacticsServerEntryIterator bool
	isTargetServerEntryIterator  bool
	hasNextTargetServerEntry     bool
	targetServerEntry            *protocol.ServerEntry
}

// NewServerEntryIterator creates a new ServerEntryIterator.
//
// The boolean return value indicates whether to treat the first server(s)
// as affinity servers or not. When the server entry selection filter changes
// such as from a specific region to any region, or when there was no previous
// filter/iterator, the the first server(s) are arbitrary and should not be
// given affinity treatment.
//
// NewServerEntryIterator and any returned ServerEntryIterator are not
// designed for concurrent use as not all related datastore operations are
// performed in a single transaction.
//
func NewServerEntryIterator(config *Config) (bool, *ServerEntryIterator, error) {

	// When configured, this target server entry is the only candidate
	if config.TargetServerEntry != "" {
		return newTargetServerEntryIterator(config, false)
	}

	filterChanged, err := hasServerEntryFilterChanged(config)
	if err != nil {
		return false, nil, common.ContextError(err)
	}

	applyServerAffinity := !filterChanged

	iterator := &ServerEntryIterator{
		config:            config,
		shuffleHeadLength: config.TunnelPoolSize,
	}

	err = iterator.Reset()
	if err != nil {
		return false, nil, common.ContextError(err)
	}

	return applyServerAffinity, iterator, nil
}

func NewTacticsServerEntryIterator(config *Config) (*ServerEntryIterator, error) {

	// When configured, this target server entry is the only candidate
	if config.TargetServerEntry != "" {
		_, iterator, err := newTargetServerEntryIterator(config, true)
		return iterator, err
	}

	iterator := &ServerEntryIterator{
		shuffleHeadLength:            0,
		isTacticsServerEntryIterator: true,
	}

	err := iterator.Reset()
	if err != nil {
		return nil, common.ContextError(err)
	}

	return iterator, nil
}

// newTargetServerEntryIterator is a helper for initializing the TargetServerEntry case
func newTargetServerEntryIterator(config *Config, isTactics bool) (bool, *ServerEntryIterator, error) {

	serverEntry, err := protocol.DecodeServerEntry(
		config.TargetServerEntry, common.GetCurrentTimestamp(), protocol.SERVER_ENTRY_SOURCE_TARGET)
	if err != nil {
		return false, nil, common.ContextError(err)
	}

	if isTactics {

		if len(serverEntry.GetSupportedTacticsProtocols()) == 0 {
			return false, nil, common.ContextError(errors.New("TargetServerEntry does not support tactics protocols"))
		}

	} else {

		if config.EgressRegion != "" && serverEntry.Region != config.EgressRegion {
			return false, nil, common.ContextError(errors.New("TargetServerEntry does not support EgressRegion"))
		}

		limitTunnelProtocols := config.clientParameters.Get().TunnelProtocols(parameters.LimitTunnelProtocols)
		if len(limitTunnelProtocols) > 0 {
			// At the ServerEntryIterator level, only limitTunnelProtocols is applied;
			// excludeIntensive is handled higher up.
			if len(serverEntry.GetSupportedProtocols(
				config.UseUpstreamProxy(), limitTunnelProtocols, false)) == 0 {
				return false, nil, common.ContextError(errors.New("TargetServerEntry does not support LimitTunnelProtocols"))
			}
		}
	}

	iterator := &ServerEntryIterator{
		isTacticsServerEntryIterator: isTactics,
		isTargetServerEntryIterator:  true,
		hasNextTargetServerEntry:     true,
		targetServerEntry:            serverEntry,
	}

	NoticeInfo("using TargetServerEntry: %s", serverEntry.IpAddress)

	return false, iterator, nil
}

// Reset a NewServerEntryIterator to the start of its cycle. The next
// call to Next will return the first server entry.
func (iterator *ServerEntryIterator) Reset() error {
	iterator.Close()

	if iterator.isTargetServerEntryIterator {
		iterator.hasNextTargetServerEntry = true
		return nil
	}

	// This query implements the Psiphon server candidate selection
	// algorithm: the first TunnelPoolSize server candidates are in rank
	// (priority) order, to favor previously successful servers; then the
	// remaining long tail is shuffled to raise up less recent candidates.

	// BoltDB implementation note:
	// We don't keep a transaction open for the duration of the iterator
	// because this would expose the following semantics to consumer code:
	//
	//     Read-only transactions and read-write transactions ... generally
	//     shouldn't be opened simultaneously in the same goroutine. This can
	//     cause a deadlock as the read-write transaction needs to periodically
	//     re-map the data file but it cannot do so while a read-only
	//     transaction is open.
	//     (https://github.com/boltdb/bolt)
	//
	// So the underlying serverEntriesBucket could change after the serverEntryIds
	// list is built.

	var serverEntryIds []string

	err := dataStoreView(func(tx *bolt.Tx) error {
		var err error
		serverEntryIds, err = getRankedServerEntries(tx)
		if err != nil {
			return err
		}

		skipServerEntryIds := make(map[string]bool)
		for _, serverEntryId := range serverEntryIds {
			skipServerEntryIds[serverEntryId] = true
		}

		bucket := tx.Bucket([]byte(serverEntriesBucket))
		cursor := bucket.Cursor()
		for key, _ := cursor.Last(); key != nil; key, _ = cursor.Prev() {
			serverEntryId := string(key)
			if _, ok := skipServerEntryIds[serverEntryId]; ok {
				continue
			}
			serverEntryIds = append(serverEntryIds, serverEntryId)
		}
		return nil
	})
	if err != nil {
		return common.ContextError(err)
	}

	for i := len(serverEntryIds) - 1; i > iterator.shuffleHeadLength-1; i-- {
		j := rand.Intn(i+1-iterator.shuffleHeadLength) + iterator.shuffleHeadLength
		serverEntryIds[i], serverEntryIds[j] = serverEntryIds[j], serverEntryIds[i]
	}

	iterator.serverEntryIds = serverEntryIds
	iterator.serverEntryIndex = 0

	return nil
}

// Close cleans up resources associated with a ServerEntryIterator.
func (iterator *ServerEntryIterator) Close() {
	iterator.serverEntryIds = nil
	iterator.serverEntryIndex = 0
}

// Next returns the next server entry, by rank, for a ServerEntryIterator.
// Returns nil with no error when there is no next item.
func (iterator *ServerEntryIterator) Next() (*protocol.ServerEntry, error) {

	var serverEntry *protocol.ServerEntry
	var err error

	defer func() {
		if err != nil {
			iterator.Close()
		}
	}()

	if iterator.isTargetServerEntryIterator {
		if iterator.hasNextTargetServerEntry {
			iterator.hasNextTargetServerEntry = false
			return MakeCompatibleServerEntry(iterator.targetServerEntry), nil
		}
		return nil, nil
	}

	// There are no region/protocol indexes for the server entries bucket.
	// Loop until we have the next server entry that matches the iterator
	// filter requirements.
	for {
		if iterator.serverEntryIndex >= len(iterator.serverEntryIds) {
			// There is no next item
			return nil, nil
		}

		serverEntryId := iterator.serverEntryIds[iterator.serverEntryIndex]
		iterator.serverEntryIndex += 1

		var data []byte

		err = dataStoreView(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte(serverEntriesBucket))
			value := bucket.Get([]byte(serverEntryId))
			if value != nil {
				// Must make a copy as slice is only valid within transaction.
				data = make([]byte, len(value))
				copy(data, value)
			}
			return nil
		})
		if err != nil {
			return nil, common.ContextError(err)
		}

		if data == nil {
			// In case of data corruption or a bug causing this condition,
			// do not stop iterating.
			NoticeAlert("ServerEntryIterator.Next: unexpected missing server entry: %s", serverEntryId)
			continue
		}

		err = json.Unmarshal(data, &serverEntry)
		if err != nil {
			// In case of data corruption or a bug causing this condition,
			// do not stop iterating.
			NoticeAlert("ServerEntryIterator.Next: %s", common.ContextError(err))
			continue
		}

		// Check filter requirements

		if iterator.isTacticsServerEntryIterator {

			// Tactics doesn't filter by egress region.
			if len(serverEntry.GetSupportedTacticsProtocols()) > 0 {
				break
			}

		} else {

			if iterator.config.EgressRegion == "" ||
				serverEntry.Region == iterator.config.EgressRegion {
				break
			}
		}
	}

	return MakeCompatibleServerEntry(serverEntry), nil
}

// MakeCompatibleServerEntry provides backwards compatibility with old server entries
// which have a single meekFrontingDomain and not a meekFrontingAddresses array.
// By copying this one meekFrontingDomain into meekFrontingAddresses, this client effectively
// uses that single value as legacy clients do.
func MakeCompatibleServerEntry(serverEntry *protocol.ServerEntry) *protocol.ServerEntry {
	if len(serverEntry.MeekFrontingAddresses) == 0 && serverEntry.MeekFrontingDomain != "" {
		serverEntry.MeekFrontingAddresses =
			append(serverEntry.MeekFrontingAddresses, serverEntry.MeekFrontingDomain)
	}

	return serverEntry
}

func scanServerEntries(scanner func(*protocol.ServerEntry)) error {
	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(serverEntriesBucket))
		cursor := bucket.Cursor()

		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			serverEntry := new(protocol.ServerEntry)
			err := json.Unmarshal(value, serverEntry)
			if err != nil {
				// In case of data corruption or a bug causing this condition,
				// do not stop iterating.
				NoticeAlert("scanServerEntries: %s", common.ContextError(err))
				continue
			}
			scanner(serverEntry)
		}

		return nil
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// CountServerEntries returns a count of stored server entries.
func CountServerEntries() int {
	count := 0
	err := scanServerEntries(func(_ *protocol.ServerEntry) {
		count += 1
	})

	if err != nil {
		NoticeAlert("CountServerEntries failed: %s", err)
		return 0
	}

	return count
}

// CountServerEntriesWithLimits returns a count of stored server entries for
// the specified region and tunnel protocol limits.
func CountServerEntriesWithLimits(
	useUpstreamProxy bool, region string, limitState *limitTunnelProtocolsState) (int, int) {

	// When CountServerEntriesWithLimits is called only
	// limitTunnelProtocolState is fixed; excludeIntensive is transitory.
	excludeIntensive := false

	initialCount := 0
	count := 0
	err := scanServerEntries(func(serverEntry *protocol.ServerEntry) {
		if region == "" || serverEntry.Region == region {

			if limitState.isInitialCandidate(excludeIntensive, serverEntry) {
				initialCount += 1
			}

			if limitState.isCandidate(excludeIntensive, serverEntry) {
				count += 1
			}

		}
	})

	if err != nil {
		NoticeAlert("CountServerEntriesWithLimits failed: %s", err)
		return 0, 0
	}

	return initialCount, count
}

// ReportAvailableRegions prints a notice with the available egress regions.
func ReportAvailableRegions(config *Config, limitState *limitTunnelProtocolsState) {

	// When ReportAvailableRegions is called only
	// limitTunnelProtocolState is fixed; excludeIntensive is transitory.
	excludeIntensive := false

	regions := make(map[string]bool)
	err := scanServerEntries(func(serverEntry *protocol.ServerEntry) {

		if limitState.isInitialCandidate(excludeIntensive, serverEntry) ||
			limitState.isCandidate(excludeIntensive, serverEntry) {

			regions[serverEntry.Region] = true
		}
	})

	if err != nil {
		NoticeAlert("ReportAvailableRegions failed: %s", err)
		return
	}

	regionList := make([]string, 0, len(regions))
	for region := range regions {
		// Some server entries do not have a region, but it makes no sense to return
		// an empty string as an "available region".
		if region != "" {
			regionList = append(regionList, region)
		}
	}

	NoticeAvailableEgressRegions(regionList)
}

// GetServerEntryIpAddresses returns an array containing
// all stored server IP addresses.
func GetServerEntryIpAddresses() ([]string, error) {

	ipAddresses := make([]string, 0)
	err := scanServerEntries(func(serverEntry *protocol.ServerEntry) {
		ipAddresses = append(ipAddresses, serverEntry.IpAddress)
	})

	if err != nil {
		return nil, common.ContextError(err)
	}

	return ipAddresses, nil
}

// SetSplitTunnelRoutes updates the cached routes data for
// the given region. The associated etag is also stored and
// used to make efficient web requests for updates to the data.
func SetSplitTunnelRoutes(region, etag string, data []byte) error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(splitTunnelRouteETagsBucket))
		err := bucket.Put([]byte(region), []byte(etag))

		bucket = tx.Bucket([]byte(splitTunnelRouteDataBucket))
		err = bucket.Put([]byte(region), data)
		return err
	})

	if err != nil {
		return common.ContextError(err)
	}
	return nil
}

// GetSplitTunnelRoutesETag retrieves the etag for cached routes
// data for the specified region. If not found, it returns an empty string value.
func GetSplitTunnelRoutesETag(region string) (string, error) {

	var etag string

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(splitTunnelRouteETagsBucket))
		etag = string(bucket.Get([]byte(region)))
		return nil
	})

	if err != nil {
		return "", common.ContextError(err)
	}
	return etag, nil
}

// GetSplitTunnelRoutesData retrieves the cached routes data
// for the specified region. If not found, it returns a nil value.
func GetSplitTunnelRoutesData(region string) ([]byte, error) {

	var data []byte

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(splitTunnelRouteDataBucket))
		value := bucket.Get([]byte(region))
		if value != nil {
			// Must make a copy as slice is only valid within transaction.
			data = make([]byte, len(value))
			copy(data, value)
		}
		return nil
	})

	if err != nil {
		return nil, common.ContextError(err)
	}
	return data, nil
}

// SetUrlETag stores an ETag for the specfied URL.
// Note: input URL is treated as a string, and is not
// encoded or decoded or otherwise canonicalized.
func SetUrlETag(url, etag string) error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(urlETagsBucket))
		err := bucket.Put([]byte(url), []byte(etag))
		return err
	})

	if err != nil {
		return common.ContextError(err)
	}
	return nil
}

// GetUrlETag retrieves a previously stored an ETag for the
// specfied URL. If not found, it returns an empty string value.
func GetUrlETag(url string) (string, error) {

	var etag string

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(urlETagsBucket))
		etag = string(bucket.Get([]byte(url)))
		return nil
	})

	if err != nil {
		return "", common.ContextError(err)
	}
	return etag, nil
}

// SetKeyValue stores a key/value pair.
func SetKeyValue(key, value string) error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(keyValueBucket))
		err := bucket.Put([]byte(key), []byte(value))
		return err
	})

	if err != nil {
		return common.ContextError(err)
	}
	return nil
}

// GetKeyValue retrieves the value for a given key. If not found,
// it returns an empty string value.
func GetKeyValue(key string) (string, error) {

	var value string

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(keyValueBucket))
		value = string(bucket.Get([]byte(key)))
		return nil
	})

	if err != nil {
		return "", common.ContextError(err)
	}
	return value, nil
}

// Persistent stat records in the persistentStatStateUnreported
// state are available for take out.
//
// Records in the persistentStatStateReporting have been taken
// out and are pending either deletion (for a successful request)
// or change to StateUnreported (for a failed request).
//
// All persistent stat records are reverted to StateUnreported
// when the datastore is initialized at start up.

var persistentStatStateUnreported = []byte("0")
var persistentStatStateReporting = []byte("1")

var persistentStatTypes = []string{
	PERSISTENT_STAT_TYPE_REMOTE_SERVER_LIST,
}

// StorePersistentStat adds a new persistent stat record, which
// is set to StateUnreported and is an immediate candidate for
// reporting.
//
// The stat is a JSON byte array containing fields as
// required by the Psiphon server API. It's assumed that the
// JSON value contains enough unique information for the value to
// function as a key in the key/value datastore. This assumption
// is currently satisfied by the fields sessionId + tunnelNumber
// for tunnel stats, and URL + ETag for remote server list stats.
func StorePersistentStat(statType string, stat []byte) error {

	if !common.Contains(persistentStatTypes, statType) {
		return common.ContextError(fmt.Errorf("invalid persistent stat type: %s", statType))
	}

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(statType))
		err := bucket.Put(stat, persistentStatStateUnreported)
		return err
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// CountUnreportedPersistentStats returns the number of persistent
// stat records in StateUnreported.
func CountUnreportedPersistentStats() int {

	unreported := 0

	err := dataStoreView(func(tx *bolt.Tx) error {

		for _, statType := range persistentStatTypes {

			bucket := tx.Bucket([]byte(statType))
			cursor := bucket.Cursor()
			for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
				if 0 == bytes.Compare(value, persistentStatStateUnreported) {
					unreported++
					break
				}
			}
		}
		return nil
	})

	if err != nil {
		NoticeAlert("CountUnreportedPersistentStats failed: %s", err)
		return 0
	}

	return unreported
}

// TakeOutUnreportedPersistentStats returns up to maxCount persistent
// stats records that are in StateUnreported. The records are set to
// StateReporting. If the records are successfully reported, clear them
// with ClearReportedPersistentStats. If the records are not successfully
// reported, restore them with PutBackUnreportedPersistentStats.
func TakeOutUnreportedPersistentStats(maxCount int) (map[string][][]byte, error) {

	stats := make(map[string][][]byte)

	err := dataStoreUpdate(func(tx *bolt.Tx) error {

		count := 0

		for _, statType := range persistentStatTypes {

			bucket := tx.Bucket([]byte(statType))
			cursor := bucket.Cursor()
			for key, value := cursor.First(); key != nil; key, value = cursor.Next() {

				if count >= maxCount {
					break
				}

				// Perform a test JSON unmarshaling. In case of data corruption or a bug,
				// skip the record.
				var jsonData interface{}
				err := json.Unmarshal(key, &jsonData)
				if err != nil {
					NoticeAlert(
						"Invalid key in TakeOutUnreportedPersistentStats: %s: %s",
						string(key), err)
					continue
				}

				if 0 == bytes.Compare(value, persistentStatStateUnreported) {
					// Must make a copy as slice is only valid within transaction.
					data := make([]byte, len(key))
					copy(data, key)

					if stats[statType] == nil {
						stats[statType] = make([][]byte, 0)
					}

					stats[statType] = append(stats[statType], data)
					count += 1
				}
			}

			for _, key := range stats[statType] {
				err := bucket.Put(key, persistentStatStateReporting)
				if err != nil {
					return err
				}
			}

		}
		return nil
	})

	if err != nil {
		return nil, common.ContextError(err)
	}

	return stats, nil
}

// PutBackUnreportedPersistentStats restores a list of persistent
// stat records to StateUnreported.
func PutBackUnreportedPersistentStats(stats map[string][][]byte) error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {

		for _, statType := range persistentStatTypes {

			bucket := tx.Bucket([]byte(statType))
			for _, key := range stats[statType] {
				err := bucket.Put(key, persistentStatStateUnreported)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// ClearReportedPersistentStats deletes a list of persistent
// stat records that were successfully reported.
func ClearReportedPersistentStats(stats map[string][][]byte) error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {

		for _, statType := range persistentStatTypes {

			bucket := tx.Bucket([]byte(statType))
			for _, key := range stats[statType] {
				err := bucket.Delete(key)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// resetAllPersistentStatsToUnreported sets all persistent stat
// records to StateUnreported. This reset is called when the
// datastore is initialized at start up, as we do not know if
// persistent records in StateReporting were reported or not.
func resetAllPersistentStatsToUnreported() error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {

		for _, statType := range persistentStatTypes {

			bucket := tx.Bucket([]byte(statType))
			resetKeys := make([][]byte, 0)
			cursor := bucket.Cursor()
			for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
				resetKeys = append(resetKeys, key)
			}
			// TODO: data mutation is done outside cursor. Is this
			// strictly necessary in this case? As is, this means
			// all stats need to be loaded into memory at once.
			// https://godoc.org/github.com/boltdb/bolt#Cursor
			for _, key := range resetKeys {
				err := bucket.Put(key, persistentStatStateUnreported)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// CountSLOKs returns the total number of SLOK records.
func CountSLOKs() int {

	count := 0

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(slokBucket))
		cursor := bucket.Cursor()
		for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
			count++
		}
		return nil
	})

	if err != nil {
		NoticeAlert("CountSLOKs failed: %s", err)
		return 0
	}

	return count
}

// DeleteSLOKs deletes all SLOK records.
func DeleteSLOKs() error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(slokBucket))
		return bucket.ForEach(
			func(id, _ []byte) error {
				return bucket.Delete(id)
			})
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

// SetSLOK stores a SLOK key, referenced by its ID. The bool
// return value indicates whether the SLOK was already stored.
func SetSLOK(id, key []byte) (bool, error) {

	var duplicate bool

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(slokBucket))
		duplicate = bucket.Get(id) != nil
		err := bucket.Put([]byte(id), []byte(key))
		return err
	})

	if err != nil {
		return false, common.ContextError(err)
	}

	return duplicate, nil
}

// GetSLOK returns a SLOK key for the specified ID. The return
// value is nil if the SLOK is not found.
func GetSLOK(id []byte) ([]byte, error) {

	var key []byte

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(slokBucket))
		key = bucket.Get(id)
		return nil
	})

	if err != nil {
		return nil, common.ContextError(err)
	}

	return key, nil
}

// TacticsStorer implements tactics.Storer.
type TacticsStorer struct {
}

func (t *TacticsStorer) SetTacticsRecord(networkID string, record []byte) error {
	return setBucketValue([]byte(tacticsBucket), []byte(networkID), record)
}

func (t *TacticsStorer) GetTacticsRecord(networkID string) ([]byte, error) {
	return getBucketValue([]byte(tacticsBucket), []byte(networkID))
}

func (t *TacticsStorer) SetSpeedTestSamplesRecord(networkID string, record []byte) error {
	return setBucketValue([]byte(speedTestSamplesBucket), []byte(networkID), record)
}

func (t *TacticsStorer) GetSpeedTestSamplesRecord(networkID string) ([]byte, error) {
	return getBucketValue([]byte(speedTestSamplesBucket), []byte(networkID))
}

// GetTacticsStorer creates a TacticsStorer.
func GetTacticsStorer() *TacticsStorer {
	return &TacticsStorer{}
}

func setBucketValue(bucket, key, value []byte) error {

	err := dataStoreUpdate(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucket)
		err := bucket.Put(key, value)
		return err
	})

	if err != nil {
		return common.ContextError(err)
	}

	return nil
}

func getBucketValue(bucket, key []byte) ([]byte, error) {

	var value []byte

	err := dataStoreView(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucket)
		value = bucket.Get(key)
		return nil
	})

	if err != nil {
		return nil, common.ContextError(err)
	}

	return value, nil
}
