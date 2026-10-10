package rds

import (
	"net/http"
	"sort"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// Manual DB snapshots. may records them (including final snapshots taken
// on delete) but copies no data, and cannot restore from them yet.

type snapshot struct {
	id, instanceID, resourceID string
	engine                     *engine
	version                    engineVersion
	storage, iops, throughput  int
	storageType                string
	port                       int
	az, vpcID                  string
	instanceCreated, created   time.Time
	masterUser, optionGroup    string
	encrypted                  bool
	kmsKey                     string
	iamAuth                    bool
	backupRetention            int
	backupWindow               string
	status                     string
	restore                    map[string]bool // accounts (or "all") allowed to restore
	tags                       tagMap
}

type xSnapshot struct {
	DBSnapshotIdentifier             string
	DBInstanceIdentifier             string
	SnapshotCreateTime               string `xml:",omitempty"`
	Engine                           string
	AllocatedStorage                 int
	Status                           string
	Port                             int
	AvailabilityZone                 string
	VpcId                            string
	InstanceCreateTime               string `xml:",omitempty"`
	MasterUsername                   string
	EngineVersion                    string
	LicenseModel                     string
	SnapshotType                     string
	Iops                             int `xml:",omitempty"`
	StorageThroughput                int `xml:",omitempty"`
	OptionGroupName                  string
	PercentProgress                  int
	StorageType                      string
	Encrypted                        bool
	BackupRetentionPeriod            int
	PreferredBackupWindow            string
	KmsKeyId                         string `xml:",omitempty"`
	DBSnapshotArn                    string
	IAMDatabaseAuthenticationEnabled bool
	ProcessorFeatures                struct{}
	DbiResourceId                    string
	TagList                          xTagList
	SnapshotTarget                   string
	OriginalSnapshotCreateTime       string `xml:",omitempty"`
	DedicatedLogVolume               bool
	MultiTenant                      bool
}

func (rs *regionState) xSnapshot(sn *snapshot) xSnapshot {
	x := xSnapshot{DBSnapshotIdentifier: sn.id, DBInstanceIdentifier: sn.instanceID, Engine: sn.engine.name,
		AllocatedStorage: sn.storage, Status: sn.status, Port: sn.port, AvailabilityZone: sn.az, VpcId: sn.vpcID,
		MasterUsername: sn.masterUser, EngineVersion: sn.version.version, LicenseModel: sn.engine.license,
		SnapshotType: "manual", Iops: sn.iops, StorageThroughput: sn.throughput, OptionGroupName: sn.optionGroup,
		StorageType: sn.storageType, Encrypted: sn.encrypted, BackupRetentionPeriod: sn.backupRetention,
		PreferredBackupWindow: sn.backupWindow, KmsKeyId: sn.kmsKey, DBSnapshotArn: awsproto.ARN("rds", rs.name, "snapshot:"+sn.id),
		IAMDatabaseAuthenticationEnabled: sn.iamAuth, DbiResourceId: sn.resourceID, TagList: sn.tags.list(), SnapshotTarget: "region"}
	if sn.status == "available" {
		x.PercentProgress = 100
		x.SnapshotCreateTime = awsproto.ISOTimeMillis(sn.created)
		x.OriginalSnapshotCreateTime = x.SnapshotCreateTime
	}
	if !sn.instanceCreated.IsZero() {
		x.InstanceCreateTime = awsproto.ISOTimeMillis(sn.instanceCreated)
	}
	return x
}

func (s *Service) registerSnapshots() {
	s.handle("CreateDBSnapshot", s.createSnapshot)
	s.handle("DescribeDBSnapshots", s.describeSnapshots)
	s.handle("DeleteDBSnapshot", s.deleteSnapshot)
	s.handle("DescribeDBSnapshotAttributes", s.describeSnapshotAttributes)
	s.handle("ModifyDBSnapshotAttribute", s.modifySnapshotAttribute)
}

// snapshotInstance records a manual snapshot of db, which becomes available
// shortly after. tags nil copies the instance's tags if CopyTagsToSnapshot
// is set.
func (s *Service) snapshotInstance(rs *regionState, db *dbInstance, id string, tags tagMap) (*snapshot, error) {
	if _, ok := rs.snapshots[id]; ok {
		return nil, errExists("DBSnapshotAlreadyExists", "Cannot create the snapshot because a snapshot with the identifier %s already exists.", id)
	}
	if tags == nil {
		tags = tagMap{}
		if db.copyTags {
			for k, v := range db.tags {
				tags[k] = v
			}
		}
	}
	vpcID := ""
	if g, ok := rs.subnets[db.subnetGroup]; ok {
		vpcID = g.vpcID
	}
	sn := &snapshot{id: id, instanceID: db.identifier, resourceID: db.resourceID, engine: db.engine, version: db.version,
		storage: db.storage, iops: db.iops, throughput: db.throughput, storageType: db.storageType, port: db.port, az: db.az,
		vpcID: vpcID, instanceCreated: db.created, masterUser: db.masterUser, optionGroup: db.optionGroup,
		encrypted: db.encrypted, kmsKey: db.kmsKey, iamAuth: db.iamAuth, backupRetention: db.backupRetention,
		backupWindow: db.backupWindow, status: "creating", restore: map[string]bool{}, tags: tags}
	rs.snapshots[id] = sn
	region := rs.name
	time.AfterFunc(fakeDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r, ok := s.regions[region]; ok && r.snapshots[id] == sn && sn.status == "creating" {
			sn.status, sn.created = "available", time.Now()
		}
	})
	return sn, nil
}

func (rs *regionState) snapshot(id string) (*snapshot, error) {
	name := nameFromARN(id, "snapshot")
	sn, ok := rs.snapshots[name]
	if !ok {
		return nil, errNotFound("DBSnapshotNotFound", "DBSnapshot %s not found.", name)
	}
	return sn, nil
}

func (s *Service) createSnapshot(rs *regionState, f awsproto.Form) (interface{}, error) {
	id, err := identifier(f, "DBSnapshotIdentifier", "DBSnapshotIdentifier")
	if err != nil {
		return nil, err
	}
	db, err := rs.instance(f.Get("DBInstanceIdentifier"))
	if err != nil {
		return nil, err
	}
	if db.status != "available" {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBInstanceState",
			"Cannot create a snapshot because the database instance %s is not currently in the available state.", db.identifier)
	}
	var tags tagMap
	if f.Has("Tags.Tag.1.Key") {
		if tags, err = newTags(f); err != nil {
			return nil, err
		}
	}
	sn, err := s.snapshotInstance(rs, db, id, tags)
	if err != nil {
		return nil, err
	}
	return struct{ DBSnapshot xSnapshot }{rs.xSnapshot(sn)}, nil
}

func (s *Service) describeSnapshots(rs *regionState, f awsproto.Form) (interface{}, error) {
	var list []*snapshot
	if id := f.Get("DBSnapshotIdentifier"); id != "" {
		sn, err := rs.snapshot(id)
		if err != nil {
			return nil, err
		}
		list = append(list, sn)
	} else {
		for _, k := range sortedKeys(rs.snapshots) {
			list = append(list, rs.snapshots[k])
		}
	}
	instance := nameFromARN(f.Get("DBInstanceIdentifier"), "db")
	typ := f.Get("SnapshotType")
	filters := parseFilters(f)
	var hits []*snapshot
	for _, sn := range list {
		if instance != "" && sn.instanceID != instance || typ != "" && typ != "manual" ||
			f.Get("DbiResourceId") != "" && f.Get("DbiResourceId") != sn.resourceID {
			continue
		}
		ok, err := matches(filters, map[string][]string{
			"db-instance-id":  {sn.instanceID, rs.instanceARN(sn.instanceID)},
			"db-snapshot-id":  {sn.id},
			"dbi-resource-id": {sn.resourceID},
			"snapshot-type":   {"manual"},
			"engine":          {sn.engine.name},
		})
		if err != nil {
			return nil, err
		}
		if ok {
			hits = append(hits, sn)
		}
	}
	page, marker, err := paginate(hits, f)
	if err != nil {
		return nil, err
	}
	out := make([]xSnapshot, len(page))
	for i, sn := range page {
		out[i] = rs.xSnapshot(sn)
	}
	return struct {
		DBSnapshots struct{ DBSnapshot []xSnapshot }
		Marker      string `xml:",omitempty"`
	}{struct{ DBSnapshot []xSnapshot }{out}, marker}, nil
}

func (s *Service) deleteSnapshot(rs *regionState, f awsproto.Form) (interface{}, error) {
	id, err := required(f, "DBSnapshotIdentifier")
	if err != nil {
		return nil, err
	}
	sn, err := rs.snapshot(id)
	if err != nil {
		return nil, err
	}
	if sn.status != "available" {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBSnapshotState", "Cannot delete the snapshot %s because it is %s.", sn.id, sn.status)
	}
	delete(rs.snapshots, sn.id)
	sn.status = "deleted"
	return struct{ DBSnapshot xSnapshot }{rs.xSnapshot(sn)}, nil
}

type xSnapshotAttribute struct {
	AttributeName   string
	AttributeValues struct{ AttributeValue []string }
}

type xSnapshotAttributes struct {
	DBSnapshotIdentifier string
	DBSnapshotAttributes struct{ DBSnapshotAttribute []xSnapshotAttribute }
}

func (sn *snapshot) xAttributes() xSnapshotAttributes {
	attr := xSnapshotAttribute{AttributeName: "restore"}
	for v := range sn.restore {
		attr.AttributeValues.AttributeValue = append(attr.AttributeValues.AttributeValue, v)
	}
	sort.Strings(attr.AttributeValues.AttributeValue)
	x := xSnapshotAttributes{DBSnapshotIdentifier: sn.id}
	x.DBSnapshotAttributes.DBSnapshotAttribute = []xSnapshotAttribute{attr}
	return x
}

func (s *Service) describeSnapshotAttributes(rs *regionState, f awsproto.Form) (interface{}, error) {
	id, err := required(f, "DBSnapshotIdentifier")
	if err != nil {
		return nil, err
	}
	sn, err := rs.snapshot(id)
	if err != nil {
		return nil, err
	}
	return struct{ DBSnapshotAttributesResult xSnapshotAttributes }{sn.xAttributes()}, nil
}

func (s *Service) modifySnapshotAttribute(rs *regionState, f awsproto.Form) (interface{}, error) {
	id, err := required(f, "DBSnapshotIdentifier")
	if err != nil {
		return nil, err
	}
	sn, err := rs.snapshot(id)
	if err != nil {
		return nil, err
	}
	if name := f.Get("AttributeName"); name != "restore" {
		return nil, errInvalid("Invalid snapshot attribute name: %s. The only supported attribute is restore.", name)
	}
	for _, v := range f.Strings("ValuesToAdd.AttributeValue") {
		sn.restore[v] = true
	}
	for _, v := range f.Strings("ValuesToRemove.AttributeValue") {
		delete(sn.restore, v)
	}
	return struct{ DBSnapshotAttributesResult xSnapshotAttributes }{sn.xAttributes()}, nil
}
