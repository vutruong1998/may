package rds

import (
	"crypto/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

type dbInstance struct {
	identifier, resourceID    string
	class, status             string
	engine                    *engine
	version                   engineVersion
	masterUser, password      string
	dbName, characterSet      string
	storage, maxStorage       int
	storageType               string
	iops, throughput          int
	encrypted                 bool
	kmsKey                    string
	port, endpointPort        int
	subnetGroup               string
	securityGroups            []string
	az, secondaryAZ           string
	multiAZ, public           bool
	paramGroup, paramStatus   string
	optionGroup               string
	backupRetention           int
	backupWindow, maintWindow string
	autoMinor, copyTags       bool
	deletionProtection        bool
	iamAuth                   bool
	perfInsights              bool
	piRetention               int
	monitoringInterval        int
	monitoringRole            string
	logExports                []string
	caCert, networkType       string
	created                   time.Time // first became available
	tags                      tagMap
}

func (rs *regionState) instanceARN(id string) string { return awsproto.ARN("rds", rs.name, "db:"+id) }

func (rs *regionState) instance(id string) (*dbInstance, error) {
	if id == "" {
		return nil, awsproto.ValidationError("1 validation error detected: Value null at 'dBInstanceIdentifier' failed to satisfy constraint: Member must not be null")
	}
	name := nameFromARN(id, "db")
	db, ok := rs.instances[name]
	if !ok {
		return nil, errNotFound("DBInstanceNotFound", "DBInstance %s not found.", name)
	}
	return db, nil
}

func errState(db *dbInstance, want string) error {
	return rdsError(http.StatusBadRequest, "InvalidDBInstanceState", "Instance %s is not %s, it is %s.", db.identifier, want, db.status)
}

// newResourceID returns a DbiResourceId such as db-ABCDEFGHIJKLMNOPQRSTUVWXYZ.
func newResourceID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 26)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "db-" + string(b)
}

func (rs *regionState) kmsKey() string {
	return awsproto.ARN("kms", rs.name, "key/00000000-0000-4000-8000-0000000a2d50") // the aws/rds key
}

// XML.

type xEndpoint struct {
	Address      string
	Port         int
	HostedZoneId string
}

type xVPCSG struct {
	VpcSecurityGroupId string
	Status             string
}

type xParamStatus struct {
	DBParameterGroupName string
	ParameterApplyStatus string
}

type xOptionGroup struct {
	OptionGroupName string
	Status          string
}

type xDBInstance struct {
	DBInstanceIdentifier               string
	DBInstanceClass                    string
	Engine                             string
	DBInstanceStatus                   string
	MasterUsername                     string
	DBName                             string     `xml:",omitempty"`
	Endpoint                           *xEndpoint `xml:",omitempty"`
	AllocatedStorage                   int
	InstanceCreateTime                 string `xml:",omitempty"`
	PreferredBackupWindow              string
	BackupRetentionPeriod              int
	DBSecurityGroups                   struct{}
	VpcSecurityGroups                  struct{ VpcSecurityGroupMembership []xVPCSG }
	DBParameterGroups                  struct{ DBParameterGroup []xParamStatus }
	AvailabilityZone                   string
	DBSubnetGroup                      *xSubnetGroup
	PreferredMaintenanceWindow         string
	PendingModifiedValues              struct{}
	LatestRestorableTime               string `xml:",omitempty"`
	MultiAZ                            bool
	EngineVersion                      string
	AutoMinorVersionUpgrade            bool
	ReadReplicaDBInstanceIdentifiers   struct{}
	LicenseModel                       string
	Iops                               int `xml:",omitempty"`
	StorageThroughput                  int `xml:",omitempty"`
	OptionGroupMemberships             struct{ OptionGroupMembership []xOptionGroup }
	CharacterSetName                   string `xml:",omitempty"`
	SecondaryAvailabilityZone          string `xml:",omitempty"`
	PubliclyAccessible                 bool
	StorageType                        string
	DbInstancePort                     int
	StorageEncrypted                   bool
	KmsKeyId                           string `xml:",omitempty"`
	DbiResourceId                      string
	CACertificateIdentifier            string
	DomainMemberships                  struct{}
	CopyTagsToSnapshot                 bool
	MonitoringInterval                 int
	MonitoringRoleArn                  string `xml:",omitempty"`
	DBInstanceArn                      string
	IAMDatabaseAuthenticationEnabled   bool
	PerformanceInsightsEnabled         bool
	PerformanceInsightsRetentionPeriod int `xml:",omitempty"`
	EnabledCloudwatchLogsExports       awsproto.Members[string]
	DeletionProtection                 bool
	AssociatedRoles                    struct{}
	MaxAllocatedStorage                int `xml:",omitempty"`
	TagList                            xTagList
	CustomerOwnedIpEnabled             bool
	NetworkType                        string
	BackupTarget                       string
	CertificateDetails                 struct{ CAIdentifier, ValidTill string }
	DedicatedLogVolume                 bool
	IsStorageConfigUpgradeAvailable    bool
	EngineLifecycleSupport             string
}

func (s *Service) xInstance(rs *regionState, db *dbInstance) *xDBInstance {
	x := &xDBInstance{
		DBInstanceIdentifier: db.identifier, DBInstanceClass: db.class, Engine: db.engine.name, DBInstanceStatus: db.status,
		MasterUsername: db.masterUser, DBName: db.dbName, AllocatedStorage: db.storage,
		PreferredBackupWindow: db.backupWindow, BackupRetentionPeriod: db.backupRetention, AvailabilityZone: db.az,
		PreferredMaintenanceWindow: db.maintWindow, MultiAZ: db.multiAZ, EngineVersion: db.version.version,
		AutoMinorVersionUpgrade: db.autoMinor, LicenseModel: db.engine.license, Iops: db.iops, StorageThroughput: db.throughput,
		CharacterSetName: db.characterSet, SecondaryAvailabilityZone: db.secondaryAZ, PubliclyAccessible: db.public,
		StorageType: db.storageType, StorageEncrypted: db.encrypted, KmsKeyId: db.kmsKey, DbiResourceId: db.resourceID,
		CACertificateIdentifier: db.caCert, CopyTagsToSnapshot: db.copyTags, MonitoringInterval: db.monitoringInterval,
		MonitoringRoleArn: db.monitoringRole, DBInstanceArn: rs.instanceARN(db.identifier),
		IAMDatabaseAuthenticationEnabled: db.iamAuth, PerformanceInsightsEnabled: db.perfInsights,
		PerformanceInsightsRetentionPeriod: db.piRetention, EnabledCloudwatchLogsExports: awsproto.List(db.logExports),
		DeletionProtection: db.deletionProtection, MaxAllocatedStorage: db.maxStorage, TagList: db.tags.list(),
		NetworkType: db.networkType, BackupTarget: "region", EngineLifecycleSupport: "open-source-rds-extended-support",
	}
	if !db.created.IsZero() {
		x.Endpoint = &xEndpoint{Address: "localhost", Port: db.endpointPort, HostedZoneId: "Z2R2ITUGPM61AM"}
		x.InstanceCreateTime = awsproto.ISOTimeMillis(db.created)
		if db.backupRetention > 0 {
			x.LatestRestorableTime = awsproto.ISOTimeMillis(time.Now().Add(-5 * time.Minute))
		}
	}
	for _, sg := range db.securityGroups {
		x.VpcSecurityGroups.VpcSecurityGroupMembership = append(x.VpcSecurityGroups.VpcSecurityGroupMembership, xVPCSG{sg, "active"})
	}
	x.DBParameterGroups.DBParameterGroup = []xParamStatus{{db.paramGroup, db.paramStatus}}
	x.OptionGroupMemberships.OptionGroupMembership = []xOptionGroup{{db.optionGroup, "in-sync"}}
	if g, ok := rs.subnets[db.subnetGroup]; ok {
		x.DBSubnetGroup = rs.xSubnetGroup(g)
		x.DBSubnetGroup.DBSubnetGroupArn = "" // not part of the instance's copy
	}
	x.CertificateDetails.CAIdentifier = db.caCert
	x.CertificateDetails.ValidTill = awsproto.ISOTimeMillis(time.Now().AddDate(1, 0, 0).Truncate(24 * time.Hour))
	return x
}

func (s *Service) registerInstances() {
	s.handle("CreateDBInstance", s.createInstance)
	s.handle("DescribeDBInstances", s.describeInstances)
	s.handle("ModifyDBInstance", s.modifyInstance)
	s.handle("DeleteDBInstance", s.deleteInstance)
	s.handle("StartDBInstance", s.startInstance)
	s.handle("StopDBInstance", s.stopInstance)
	s.handle("RebootDBInstance", s.rebootInstance)
	s.handle("DescribeDBInstanceAutomatedBackups", s.describeAutomatedBackups)
}

// spec is what the driver needs to run db. Callers hold s.mu.
func (s *Service) spec(rs *regionState, db *dbInstance) *Spec {
	ic, _ := parseClass(db.class)
	sp := &Spec{Region: rs.name, Key: db.resourceID, Identifier: db.identifier, Engine: db.engine.name,
		Version: db.version.version, Image: db.engine.image(db.version), Port: db.port, MasterUser: db.masterUser,
		MasterPassword: db.password, DBName: db.dbName, VCPUs: ic.vcpus, MemoryMiB: ic.memMiB, Params: map[string]string{}}
	if g := rs.paramGroup(db.paramGroup); g != nil {
		sp.Params = g.values()
	}
	return sp
}

// Validation.

var (
	userRE    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	dbNameRE  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	windowRE  = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`)
	maintRE   = regexp.MustCompile(`^(mon|tue|wed|thu|fri|sat|sun):([01]\d|2[0-3]):[0-5]\d-(mon|tue|wed|thu|fri|sat|sun):([01]\d|2[0-3]):[0-5]\d$`)
	reserved  = map[string]bool{"rdsadmin": true, "rdsrepladmin": true, "rds_superuser": true}
	monitorOK = map[int]bool{0: true, 1: true, 5: true, 10: true, 15: true, 30: true, 60: true}
)

func checkPassword(e *engine, p string) error {
	max := 128
	if e.name != "postgres" {
		max = 41
	}
	switch {
	case len(p) < 8:
		return errInvalid("The parameter MasterUserPassword is not a valid password because it is shorter than 8 characters.")
	case len(p) > max:
		return errInvalid("The parameter MasterUserPassword is not a valid password because it is longer than %d characters.", max)
	}
	for _, r := range p {
		if r < 0x21 || r > 0x7e || r == '/' || r == '@' || r == '"' {
			return errInvalid("The parameter MasterUserPassword is not a valid password. Only printable ASCII characters besides '/', '@', '\"', ' ' may be used.")
		}
	}
	return nil
}

func checkUser(e *engine, u string) error {
	max := 63
	if e.name != "postgres" {
		max = 32
	}
	if !userRE.MatchString(u) || len(u) > max {
		return errInvalid("The parameter MasterUsername is not a valid identifier. It must start with a letter, contain only letters, digits and underscores, and be at most %d characters.", max)
	}
	if reserved[strings.ToLower(u)] {
		return errInvalid("MasterUsername %s cannot be used as it is a reserved word used by the engine", u)
	}
	return nil
}

func checkStorage(storageType string, gib, iops int) error {
	switch storageType {
	case "standard", "gp2", "gp3":
		if gib < 20 || gib > 65536 {
			return errInvalid("Invalid storage size for engine and storage type %s: %d (it must be between 20 and 65536 GiB)", storageType, gib)
		}
	case "io1", "io2":
		if gib < 100 || gib > 65536 {
			return errInvalid("Invalid storage size for engine and storage type %s: %d (it must be between 100 and 65536 GiB)", storageType, gib)
		}
		if iops < 1000 {
			return errCombination("Provisioned IOPS (at least 1000) is required for storage type %s.", storageType)
		}
	default:
		return errInvalid("Invalid storage type: %s", storageType)
	}
	if iops > 0 && (storageType == "standard" || storageType == "gp2") {
		return errCombination("You can't specify IOPS or storage throughput for engine and storage type %s.", storageType)
	}
	return nil
}

// gp3Defaults fills in the baseline performance gp3 volumes report.
func gp3Defaults(db *dbInstance, iopsSet, throughputSet bool) {
	if db.storageType != "gp3" {
		if !iopsSet {
			db.iops = 0
		}
		db.throughput = 0
		return
	}
	baseIOPS, baseTP := 3000, 125
	if db.storage >= 400 {
		baseIOPS, baseTP = 12000, 500
	}
	if !iopsSet {
		db.iops = baseIOPS
	}
	if !throughputSet {
		db.throughput = baseTP
	}
}

// securityGroups validates VpcSecurityGroupIds against the instance's VPC,
// defaulting to the VPC's default group.
func (s *Service) securityGroups(rs *regionState, f awsproto.Form, vpcID string) ([]string, error) {
	ids := f.Strings("VpcSecurityGroupIds.VpcSecurityGroupId")
	if len(ids) == 0 {
		if sg := s.net.DefaultSecurityGroup(rs.name, vpcID); sg != "" {
			return []string{sg}, nil
		}
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		v, ok := s.net.LookupSecurityGroup(rs.name, id)
		if !ok {
			return nil, errInvalid("Invalid security group , groupId= %s, groupName=.", id)
		}
		if v != vpcID {
			return nil, errCombination("The DB instance and EC2 security group are in different VPCs. The DB instance is in %s and the EC2 security group is in %s", vpcID, v)
		}
		out = append(out, id)
	}
	return out, nil
}

func (rs *regionState) checkParamGroup(name string, v engineVersion) (*parameterGroup, error) {
	g := rs.paramGroup(name)
	if g == nil {
		return nil, errNotFound("DBParameterGroupNotFound", "DBParameterGroup not found: %s", strings.ToLower(name))
	}
	if g.family != v.family {
		return nil, errCombination("The parameter group %s with DBParameterGroupFamily %s can't be used for this instance. Use a parameter group with DBParameterGroupFamily %s.", g.name, g.family, v.family)
	}
	return g, nil
}

func defaultOptionGroup(e *engine, v engineVersion) string {
	return "default:" + e.name + "-" + strings.ReplaceAll(v.major, ".", "-")
}

func (s *Service) checkOptionGroup(f awsproto.Form, e *engine, v engineVersion) (string, error) {
	def := defaultOptionGroup(e, v)
	if name := f.Get("OptionGroupName"); name != "" && name != def {
		return "", errNotFound("OptionGroupNotFoundFault", "Specified OptionGroupName: %s not found (may only provides the default option group %s).", name, def)
	}
	return def, nil
}

func checkLogTypes(e *engine, types []string) error {
	for _, t := range types {
		ok := false
		for _, lt := range e.logTypes {
			ok = ok || lt == t
		}
		if !ok {
			return errInvalid("You cannot use the log types '%s' with engine version %s. For supported log types, see the documentation.", t, e.name)
		}
	}
	return nil
}

func (s *Service) createInstance(rs *regionState, f awsproto.Form) (interface{}, error) {
	id, err := identifier(f, "DBInstanceIdentifier", "DBInstanceIdentifier")
	if err != nil {
		return nil, err
	}
	if _, ok := rs.instances[id]; ok {
		return nil, errExists("DBInstanceAlreadyExists", "DB instance already exists")
	}
	class, err := required(f, "DBInstanceClass")
	if err != nil {
		return nil, err
	}
	name, err := required(f, "Engine")
	if err != nil {
		return nil, err
	}
	if f.Get("DBClusterIdentifier") != "" || strings.HasPrefix(name, "aurora") {
		return nil, errCombination("may does not support Aurora or DB clusters yet; use the postgres, mysql or mariadb engine.")
	}
	e, err := lookupEngine(name)
	if err != nil {
		return nil, err
	}
	if _, ok := parseClass(class); !ok {
		return nil, errCombination("RDS does not support creating a DB instance with the following combination: DBInstanceClass=%s, Engine=%s.", class, e.name)
	}
	v, err := e.resolve(f.Get("EngineVersion"))
	if err != nil {
		return nil, err
	}
	db := &dbInstance{identifier: id, resourceID: newResourceID(), class: class, status: "creating", engine: e, version: v,
		storageType: "gp2", autoMinor: f.Bool("AutoMinorVersionUpgrade", true), copyTags: f.Bool("CopyTagsToSnapshot", false),
		deletionProtection: f.Bool("DeletionProtection", false), iamAuth: f.Bool("EnableIAMDatabaseAuthentication", false),
		multiAZ: f.Bool("MultiAZ", false), caCert: "rds-ca-rsa2048-g1", networkType: "IPV4", paramStatus: "in-sync",
		characterSet: f.Get("CharacterSetName")}

	if db.storage, err = intParam(f, "AllocatedStorage", 0, 0, 1<<20); err != nil {
		return nil, err
	}
	if !f.Has("AllocatedStorage") {
		return nil, errInvalid("The parameter AllocatedStorage must be provided and must not be null.")
	}
	if t := f.Get("StorageType"); t != "" {
		db.storageType = t
	}
	if db.iops, err = intParam(f, "Iops", 0, 0, 256000); err != nil {
		return nil, err
	}
	if db.throughput, err = intParam(f, "StorageThroughput", 0, 0, 4000); err != nil {
		return nil, err
	}
	if err := checkStorage(db.storageType, db.storage, db.iops); err != nil {
		return nil, err
	}
	gp3Defaults(db, f.Has("Iops"), f.Has("StorageThroughput"))
	if db.maxStorage, err = intParam(f, "MaxAllocatedStorage", 0, 0, 65536); err != nil {
		return nil, err
	}
	if db.maxStorage != 0 && db.maxStorage < db.storage {
		return nil, errCombination("Max storage size must be greater than storage size")
	}

	if f.Bool("ManageMasterUserPassword", false) {
		return nil, errCombination("may does not support ManageMasterUserPassword (Secrets Manager) yet; set MasterUserPassword instead.")
	}
	if db.masterUser, err = required(f, "MasterUsername"); err != nil {
		return nil, err
	}
	if err := checkUser(e, db.masterUser); err != nil {
		return nil, err
	}
	if db.password = f.Get("MasterUserPassword"); db.password == "" {
		return nil, errInvalid("The parameter MasterUserPassword must be provided and must not be blank.")
	}
	if err := checkPassword(e, db.password); err != nil {
		return nil, err
	}
	if db.dbName = f.Get("DBName"); db.dbName != "" && (!dbNameRE.MatchString(db.dbName) || len(db.dbName) > 63) {
		return nil, errInvalid("The parameter DBName is not a valid identifier. It must begin with a letter and contain only letters, digits and underscores (at most 63).")
	}

	if db.port, err = intParam(f, "Port", e.port, 1150, 65535); err != nil {
		return nil, err
	}
	db.endpointPort = db.port

	var group *subnetGroup
	if n := f.Get("DBSubnetGroupName"); n != "" {
		if group, err = rs.subnetGroup(n); err != nil {
			return nil, err
		}
	} else if group, err = s.defaultSubnetGroup(rs); err != nil {
		return nil, err
	}
	db.subnetGroup = group.name
	db.public = f.Bool("PubliclyAccessible", group.name == "default")
	if db.securityGroups, err = s.securityGroups(rs, f, group.vpcID); err != nil {
		return nil, err
	}
	if err := placeAZ(db, group, f.Get("AvailabilityZone")); err != nil {
		return nil, err
	}

	pg := defaultParamGroupName(v.family)
	if n := f.Get("DBParameterGroupName"); n != "" {
		pg = n
	}
	g, err := rs.checkParamGroup(pg, v)
	if err != nil {
		return nil, err
	}
	db.paramGroup = g.name
	if db.optionGroup, err = s.checkOptionGroup(f, e, v); err != nil {
		return nil, err
	}

	if db.backupRetention, err = intParam(f, "BackupRetentionPeriod", 1, 0, 35); err != nil {
		return nil, err
	}
	db.backupWindow, db.maintWindow = "03:00-03:30", "sun:05:00-sun:05:30"
	if w := f.Get("PreferredBackupWindow"); w != "" {
		if !windowRE.MatchString(w) {
			return nil, errInvalid("The backup window must be in the format hh24:mi-hh24:mi.")
		}
		db.backupWindow = w
	}
	if w := strings.ToLower(f.Get("PreferredMaintenanceWindow")); w != "" {
		if !maintRE.MatchString(w) {
			return nil, errInvalid("The maintenance window must be in the format ddd:hh24:mi-ddd:hh24:mi.")
		}
		db.maintWindow = w
	}

	db.encrypted = f.Bool("StorageEncrypted", false)
	if k := f.Get("KmsKeyId"); k != "" {
		if !db.encrypted {
			return nil, errCombination("Encryption must be enabled when a KMS key is specified.")
		}
		db.kmsKey = k
	} else if db.encrypted {
		db.kmsKey = rs.kmsKey()
	}

	if db.monitoringInterval, err = intParam(f, "MonitoringInterval", 0, 0, 60); err != nil {
		return nil, err
	}
	if !monitorOK[db.monitoringInterval] {
		return nil, errInvalid("Unsupported MonitoringInterval: %d. Valid values are 0, 1, 5, 10, 15, 30, 60.", db.monitoringInterval)
	}
	db.monitoringRole = f.Get("MonitoringRoleArn")
	if db.monitoringInterval > 0 && db.monitoringRole == "" {
		return nil, errCombination("A MonitoringRoleARN value is required if you specify a MonitoringInterval value other than 0.")
	}
	db.perfInsights = f.Bool("EnablePerformanceInsights", false)
	if db.perfInsights {
		if db.piRetention, err = intParam(f, "PerformanceInsightsRetentionPeriod", 7, 7, 731); err != nil {
			return nil, err
		}
	}
	db.logExports = f.Strings("EnableCloudwatchLogsExports.member")
	if err := checkLogTypes(e, db.logExports); err != nil {
		return nil, err
	}
	if c := f.Get("CACertificateIdentifier"); c != "" {
		db.caCert = c
	}
	if db.tags, err = newTags(f); err != nil {
		return nil, err
	}

	rs.instances[id] = db
	s.driver.Create(s.spec(rs, db))
	return struct{ DBInstance *xDBInstance }{s.xInstance(rs, db)}, nil
}

// placeAZ picks the instance's availability zone (and the standby's for
// Multi-AZ) among the subnet group's.
func placeAZ(db *dbInstance, g *subnetGroup, want string) error {
	var zones []string
	seen := map[string]bool{}
	for _, sn := range g.subnets {
		if !seen[sn.AZ] {
			seen[sn.AZ] = true
			zones = append(zones, sn.AZ)
		}
	}
	switch {
	case want != "" && db.multiAZ:
		return errCombination("Requesting a specific availability zone is not valid for Multi-AZ instances.")
	case want != "" && !seen[want]:
		return errCombination("Cannot find the availability zone %s in the DB subnet group %s.", want, g.name)
	case want != "":
		db.az = want
	case len(zones) > 0:
		db.az = zones[0]
	}
	db.secondaryAZ = ""
	if db.multiAZ {
		for _, z := range zones {
			if z != db.az {
				db.secondaryAZ = z
				break
			}
		}
	}
	return nil
}

func instanceFields(rs *regionState, db *dbInstance) map[string][]string {
	return map[string][]string{
		"db-instance-id":  {db.identifier, rs.instanceARN(db.identifier)},
		"dbi-resource-id": {db.resourceID},
		"engine":          {db.engine.name},
		"db-cluster-id":   nil,
		"domain":          nil,
	}
}

func (s *Service) describeInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	var list []*dbInstance
	if id := f.Get("DBInstanceIdentifier"); id != "" {
		db, err := rs.instance(id)
		if err != nil {
			return nil, err
		}
		list = append(list, db)
	} else {
		for _, k := range sortedKeys(rs.instances) {
			list = append(list, rs.instances[k])
		}
	}
	filters := parseFilters(f)
	var hits []*dbInstance
	for _, db := range list {
		ok, err := matches(filters, instanceFields(rs, db))
		if err != nil {
			return nil, err
		}
		if ok {
			hits = append(hits, db)
		}
	}
	page, marker, err := paginate(hits, f)
	if err != nil {
		return nil, err
	}
	out := make([]*xDBInstance, len(page))
	for i, db := range page {
		out[i] = s.xInstance(rs, db)
	}
	return struct {
		DBInstances struct{ DBInstance []*xDBInstance }
		Marker      string `xml:",omitempty"`
	}{struct{ DBInstance []*xDBInstance }{out}, marker}, nil
}

// modifyInstance applies every change immediately, as with
// ApplyImmediately=true; may has no maintenance windows.
func (s *Service) modifyInstance(rs *regionState, f awsproto.Form) (interface{}, error) {
	db, err := rs.instance(f.Get("DBInstanceIdentifier"))
	if err != nil {
		return nil, err
	}
	if db.status != "available" {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBInstanceState", "Database instance is not in available state.")
	}
	next := *db // validate everything before changing anything
	var ch Changes
	busy := false // the change takes the instance through "modifying"

	if f.Bool("ManageMasterUserPassword", false) {
		return nil, errCombination("may does not support ManageMasterUserPassword (Secrets Manager) yet; set MasterUserPassword instead.")
	}
	newID := ""
	if f.Get("NewDBInstanceIdentifier") != "" {
		if newID, err = identifier(f, "NewDBInstanceIdentifier", "NewDBInstanceIdentifier"); err != nil {
			return nil, err
		}
		if _, ok := rs.instances[newID]; ok && newID != db.identifier {
			return nil, errExists("DBInstanceAlreadyExists", "DB instance already exists")
		}
	}
	if c := f.Get("DBInstanceClass"); c != "" && c != db.class {
		if _, ok := parseClass(c); !ok {
			return nil, errCombination("RDS does not support creating a DB instance with the following combination: DBInstanceClass=%s, Engine=%s.", c, db.engine.name)
		}
		next.class, ch.Resources, busy = c, true, true
	}
	if t := f.Get("StorageType"); t != "" && t != db.storageType {
		next.storageType, busy = t, true
	}
	if f.Has("AllocatedStorage") {
		if next.storage, err = intParam(f, "AllocatedStorage", 0, 0, 1<<20); err != nil {
			return nil, err
		}
		if next.storage < db.storage {
			return nil, errInvalid("Invalid storage size for engine name %s and storage type %s: %d (storage can't be decreased)", db.engine.name, next.storageType, next.storage)
		}
		busy = busy || next.storage != db.storage
	}
	if f.Has("Iops") {
		if next.iops, err = intParam(f, "Iops", 0, 0, 256000); err != nil {
			return nil, err
		}
	}
	if f.Has("StorageThroughput") {
		if next.throughput, err = intParam(f, "StorageThroughput", 0, 0, 4000); err != nil {
			return nil, err
		}
	}
	if err := checkStorage(next.storageType, next.storage, next.iops); err != nil {
		return nil, err
	}
	if next.storageType != db.storageType || next.storage != db.storage {
		gp3Defaults(&next, f.Has("Iops"), f.Has("StorageThroughput"))
	}
	if f.Has("MaxAllocatedStorage") {
		if next.maxStorage, err = intParam(f, "MaxAllocatedStorage", 0, 0, 65536); err != nil {
			return nil, err
		}
	}
	if next.maxStorage != 0 && next.maxStorage < next.storage {
		return nil, errCombination("Max storage size must be greater than storage size")
	}
	if p := f.Get("MasterUserPassword"); p != "" {
		if err := checkPassword(db.engine, p); err != nil {
			return nil, err
		}
		next.password, ch.Password, busy = p, true, true
	}
	if ev := f.Get("EngineVersion"); ev != "" {
		v, err := db.engine.resolve(ev)
		if err != nil {
			return nil, err
		}
		if v.version != db.version.version {
			if v.major != db.version.major {
				if !f.Bool("AllowMajorVersionUpgrade", false) {
					return nil, errCombination("The AllowMajorVersionUpgrade flag must be present when upgrading to a new major version.")
				}
				if s.driver.Name() == "docker" {
					return nil, errCombination("may cannot upgrade a running %s container from %s to %s; create a new DB instance instead.", db.engine.name, db.version.major, v.major)
				}
				if !f.Has("DBParameterGroupName") {
					next.paramGroup = defaultParamGroupName(v.family)
				}
				next.optionGroup = defaultOptionGroup(db.engine, v)
			}
			next.version, busy = v, true
		}
	}
	if n := f.Get("DBParameterGroupName"); n != "" {
		next.paramGroup = n
	}
	if next.paramGroup != db.paramGroup {
		g, err := rs.checkParamGroup(next.paramGroup, next.version)
		if err != nil {
			return nil, err
		}
		next.paramGroup = g.name
		if s.driver.Name() == "docker" {
			next.paramStatus = "pending-reboot"
		}
	}
	if n := f.Get("OptionGroupName"); n != "" && n != next.optionGroup {
		return nil, errNotFound("OptionGroupNotFoundFault", "Specified OptionGroupName: %s not found (may only provides the default option group %s).", n, next.optionGroup)
	}
	if f.Has("DBPortNumber") {
		if next.port, err = intParam(f, "DBPortNumber", db.port, 1150, 65535); err != nil {
			return nil, err
		}
		if next.port != db.port {
			next.endpointPort, ch.Port, busy = next.port, true, true
		}
	}
	group, ok := rs.subnets[next.subnetGroup]
	if n := f.Get("DBSubnetGroupName"); n != "" && strings.ToLower(n) != db.subnetGroup {
		if group, err = rs.subnetGroup(n); err != nil {
			return nil, err
		}
		next.subnetGroup, ok, busy = group.name, true, true
	}
	if ok && (f.Has("VpcSecurityGroupIds.VpcSecurityGroupId.1") || next.subnetGroup != db.subnetGroup) {
		if next.securityGroups, err = s.securityGroups(rs, f, group.vpcID); err != nil {
			return nil, err
		}
	}
	if f.Has("MultiAZ") {
		next.multiAZ = f.Bool("MultiAZ", false)
	}
	if ok && (next.multiAZ != db.multiAZ || next.subnetGroup != db.subnetGroup) {
		az := next.az
		if next.subnetGroup != db.subnetGroup {
			az = ""
		}
		if err := placeAZ(&next, group, az); err != nil {
			return nil, err
		}
		busy = true
	}
	if f.Has("BackupRetentionPeriod") {
		if next.backupRetention, err = intParam(f, "BackupRetentionPeriod", 1, 0, 35); err != nil {
			return nil, err
		}
	}
	if w := f.Get("PreferredBackupWindow"); w != "" {
		if !windowRE.MatchString(w) {
			return nil, errInvalid("The backup window must be in the format hh24:mi-hh24:mi.")
		}
		next.backupWindow = w
	}
	if w := strings.ToLower(f.Get("PreferredMaintenanceWindow")); w != "" {
		if !maintRE.MatchString(w) {
			return nil, errInvalid("The maintenance window must be in the format ddd:hh24:mi-ddd:hh24:mi.")
		}
		next.maintWindow = w
	}
	next.autoMinor = f.Bool("AutoMinorVersionUpgrade", db.autoMinor)
	next.copyTags = f.Bool("CopyTagsToSnapshot", db.copyTags)
	next.deletionProtection = f.Bool("DeletionProtection", db.deletionProtection)
	next.iamAuth = f.Bool("EnableIAMDatabaseAuthentication", db.iamAuth)
	next.public = f.Bool("PubliclyAccessible", db.public)
	if f.Has("MonitoringInterval") {
		if next.monitoringInterval, err = intParam(f, "MonitoringInterval", 0, 0, 60); err != nil {
			return nil, err
		}
		if !monitorOK[next.monitoringInterval] {
			return nil, errInvalid("Unsupported MonitoringInterval: %d. Valid values are 0, 1, 5, 10, 15, 30, 60.", next.monitoringInterval)
		}
	}
	if f.Has("MonitoringRoleArn") {
		next.monitoringRole = f.Get("MonitoringRoleArn")
	}
	if next.monitoringInterval > 0 && next.monitoringRole == "" {
		return nil, errCombination("A MonitoringRoleARN value is required if you specify a MonitoringInterval value other than 0.")
	}
	if f.Has("EnablePerformanceInsights") {
		next.perfInsights = f.Bool("EnablePerformanceInsights", false)
		next.piRetention = 0
		if next.perfInsights {
			next.piRetention = 7
		}
	}
	if f.Has("PerformanceInsightsRetentionPeriod") && next.perfInsights {
		if next.piRetention, err = intParam(f, "PerformanceInsightsRetentionPeriod", 7, 7, 731); err != nil {
			return nil, err
		}
	}
	enable := f.Strings("CloudwatchLogsExportConfiguration.EnableLogTypes.member")
	disable := f.Strings("CloudwatchLogsExportConfiguration.DisableLogTypes.member")
	if err := checkLogTypes(db.engine, append(append([]string(nil), enable...), disable...)); err != nil {
		return nil, err
	}
	if len(enable)+len(disable) > 0 {
		set := map[string]bool{}
		for _, t := range db.logExports {
			set[t] = true
		}
		for _, t := range enable {
			set[t] = true
		}
		for _, t := range disable {
			delete(set, t)
		}
		next.logExports = nil
		for _, t := range db.engine.logTypes {
			if set[t] {
				next.logExports = append(next.logExports, t)
			}
		}
	}
	if c := f.Get("CACertificateIdentifier"); c != "" {
		next.caCert = c
	}

	*db = next
	if newID != "" && newID != db.identifier {
		delete(rs.instances, db.identifier)
		db.identifier = newID
		rs.instances[newID] = db
		s.driver.Rename(db.resourceID, newID)
	}
	if busy {
		db.status = "modifying"
		s.driver.Modify(s.spec(rs, db), ch)
	}
	return struct{ DBInstance *xDBInstance }{s.xInstance(rs, db)}, nil
}

func (s *Service) deleteInstance(rs *regionState, f awsproto.Form) (interface{}, error) {
	db, err := rs.instance(f.Get("DBInstanceIdentifier"))
	if err != nil {
		return nil, err
	}
	if db.status == "deleting" {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBInstanceState", "Instance %s is already being deleted.", db.identifier)
	}
	if db.deletionProtection {
		return nil, errCombination("Cannot delete protected DB Instance, please disable deletion protection and try again.")
	}
	skip, final := f.Bool("SkipFinalSnapshot", false), f.Get("FinalDBSnapshotIdentifier")
	switch {
	case skip && final != "":
		return nil, errCombination("Cannot specify both SkipFinalSnapshot and FinalDBSnapshotIdentifier.")
	case !skip && final == "":
		return nil, errCombination("FinalDBSnapshotIdentifier is required unless SkipFinalSnapshot is specified.")
	case final != "":
		if _, err := s.snapshotInstance(rs, db, final, nil); err != nil {
			return nil, err
		}
	}
	db.status = "deleting"
	s.driver.Delete(rs.name, db.resourceID)
	return struct{ DBInstance *xDBInstance }{s.xInstance(rs, db)}, nil
}

func (s *Service) startInstance(rs *regionState, f awsproto.Form) (interface{}, error) {
	db, err := rs.instance(f.Get("DBInstanceIdentifier"))
	if err != nil {
		return nil, err
	}
	if db.status != "stopped" {
		return nil, errState(db, "stopped")
	}
	db.status = "starting"
	s.driver.Start(s.spec(rs, db))
	return struct{ DBInstance *xDBInstance }{s.xInstance(rs, db)}, nil
}

func (s *Service) stopInstance(rs *regionState, f awsproto.Form) (interface{}, error) {
	db, err := rs.instance(f.Get("DBInstanceIdentifier"))
	if err != nil {
		return nil, err
	}
	if db.status != "available" {
		return nil, errState(db, "available")
	}
	if snap := f.Get("DBSnapshotIdentifier"); snap != "" {
		if _, err := s.snapshotInstance(rs, db, snap, nil); err != nil {
			return nil, err
		}
	}
	db.status = "stopping"
	s.driver.Stop(rs.name, db.resourceID)
	return struct{ DBInstance *xDBInstance }{s.xInstance(rs, db)}, nil
}

func (s *Service) rebootInstance(rs *regionState, f awsproto.Form) (interface{}, error) {
	db, err := rs.instance(f.Get("DBInstanceIdentifier"))
	if err != nil {
		return nil, err
	}
	// A reboot also retries an instance whose parameters failed to apply.
	if db.status != "available" && db.status != "incompatible-parameters" {
		return nil, errState(db, "available")
	}
	if f.Bool("ForceFailover", false) && !db.multiAZ {
		return nil, errCombination("Cannot force a failover for a DB instance that is not Multi-AZ.")
	}
	db.status, db.paramStatus = "rebooting", "in-sync"
	s.driver.Reboot(s.spec(rs, db))
	return struct{ DBInstance *xDBInstance }{s.xInstance(rs, db)}, nil
}

// may keeps no automated backups.
func (s *Service) describeAutomatedBackups(rs *regionState, f awsproto.Form) (interface{}, error) {
	return struct {
		DBInstanceAutomatedBackups struct{}
	}{}, nil
}
