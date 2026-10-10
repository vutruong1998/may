package dynamodb

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// Wire shapes shared by table operations.

type AttributeDefinition struct {
	AttributeName string
	AttributeType string
}

type KeySchemaElement struct {
	AttributeName string
	KeyType       string
}

type Projection struct {
	ProjectionType   string   `json:",omitempty"`
	NonKeyAttributes []string `json:",omitempty"`
}

type ProvisionedThroughput struct {
	ReadCapacityUnits  *int64
	WriteCapacityUnits *int64
}

type OnDemandThroughput struct {
	MaxReadRequestUnits  *int64 `json:",omitempty"`
	MaxWriteRequestUnits *int64 `json:",omitempty"`
}

type StreamSpecification struct {
	StreamEnabled  bool
	StreamViewType string `json:",omitempty"`
}

type SSESpecification struct {
	Enabled        *bool
	SSEType        string
	KMSMasterKeyId string
}

type TimeToLiveSpecification struct {
	Enabled       bool
	AttributeName string
}

type Tag struct {
	Key   string
	Value string
}

type indexInput struct {
	IndexName             string
	KeySchema             []KeySchemaElement
	Projection            *Projection
	ProvisionedThroughput *ProvisionedThroughput
	OnDemandThroughput    *OnDemandThroughput
}

type createTableInput struct {
	TableName                 string
	AttributeDefinitions      []AttributeDefinition
	KeySchema                 []KeySchemaElement
	LocalSecondaryIndexes     []indexInput
	GlobalSecondaryIndexes    []indexInput
	BillingMode               string
	ProvisionedThroughput     *ProvisionedThroughput
	StreamSpecification       *StreamSpecification
	SSESpecification          *SSESpecification
	Tags                      []Tag
	TableClass                string
	DeletionProtectionEnabled bool
	OnDemandThroughput        *OnDemandThroughput
}

type tableNameInput struct {
	TableName string
}

type updateTableInput struct {
	TableName                   string
	AttributeDefinitions        []AttributeDefinition
	BillingMode                 string
	ProvisionedThroughput       *ProvisionedThroughput
	GlobalSecondaryIndexUpdates []struct {
		Create *indexInput
		Update *indexInput
		Delete *struct{ IndexName string }
	}
	StreamSpecification       *StreamSpecification
	SSESpecification          *SSESpecification
	TableClass                string
	DeletionProtectionEnabled *bool
	OnDemandThroughput        *OnDemandThroughput
	ReplicaUpdates            []interface{}
}

// Output shapes.

type throughputDesc struct {
	LastIncreaseDateTime   *float64 `json:",omitempty"`
	LastDecreaseDateTime   *float64 `json:",omitempty"`
	NumberOfDecreasesToday int64
	ReadCapacityUnits      int64
	WriteCapacityUnits     int64
}

type indexDesc struct {
	IndexName             string
	KeySchema             []KeySchemaElement
	Projection            Projection
	IndexStatus           string          `json:",omitempty"`
	Backfilling           *bool           `json:",omitempty"`
	ProvisionedThroughput *throughputDesc `json:",omitempty"`
	IndexSizeBytes        int64
	ItemCount             int64
	IndexArn              string
}

type tableDesc struct {
	AttributeDefinitions      []AttributeDefinition
	TableName                 string
	KeySchema                 []KeySchemaElement
	TableStatus               string
	CreationDateTime          float64
	ProvisionedThroughput     throughputDesc
	TableSizeBytes            int64
	ItemCount                 int64
	TableArn                  string
	TableId                   string
	BillingModeSummary        *billingSummary              `json:",omitempty"`
	LocalSecondaryIndexes     []indexDesc                  `json:",omitempty"`
	GlobalSecondaryIndexes    []indexDesc                  `json:",omitempty"`
	StreamSpecification       *StreamSpecification         `json:",omitempty"`
	LatestStreamLabel         string                       `json:",omitempty"`
	LatestStreamArn           string                       `json:",omitempty"`
	SSEDescription            *sseDesc                     `json:",omitempty"`
	TableClassSummary         *struct{ TableClass string } `json:",omitempty"`
	DeletionProtectionEnabled bool
	WarmThroughput            *warmThroughput `json:",omitempty"`
}

type billingSummary struct {
	BillingMode                       string
	LastUpdateToPayPerRequestDateTime *float64 `json:",omitempty"`
}

type sseDesc struct {
	Status          string
	SSEType         string
	KMSMasterKeyArn string `json:",omitempty"`
}

type warmThroughput struct {
	ReadUnitsPerSecond  int64
	WriteUnitsPerSecond int64
	Status              string
}

func (s *Service) registerTables() {
	handle(s, "CreateTable", s.createTable)
	handle(s, "DescribeTable", s.describeTable)
	handle(s, "DeleteTable", s.deleteTable)
	handle(s, "UpdateTable", s.updateTable)
	handle(s, "ListTables", s.listTables)
	handle(s, "TagResource", s.tagResource)
	handle(s, "UntagResource", s.untagResource)
	handle(s, "ListTagsOfResource", s.listTags)
	handle(s, "UpdateTimeToLive", s.updateTTL)
	handle(s, "DescribeTimeToLive", s.describeTTL)
	handle(s, "DescribeContinuousBackups", s.describeBackups)
	handle(s, "UpdateContinuousBackups", s.updateBackups)
	handle(s, "DescribeLimits", s.describeLimits)
	handle(s, "DescribeEndpoints", s.describeEndpoints)
}

var tableNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func checkTableName(name, field string) error {
	switch {
	case name == "":
		return validation("1 validation error detected: Value null at '%s' failed to satisfy constraint: Member must not be null", field)
	case len(name) < 3:
		return validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length greater than or equal to 3", name, field)
	case len(name) > 255:
		return validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length less than or equal to 255", name, field)
	case !tableNamePattern.MatchString(name):
		return validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+", name, field)
	}
	return nil
}

func parseKeySchema(ks []KeySchemaElement, where string) (keySchema, error) {
	if len(ks) == 0 || len(ks) > 2 {
		return keySchema{}, validation("1 validation error detected: Value at '%s' failed to satisfy constraint: Member must have length less than or equal to 2", where)
	}
	if ks[0].KeyType != "HASH" {
		return keySchema{}, invalidParam("Invalid KeySchema: The first KeySchemaElement is not a HASH key type")
	}
	k := keySchema{hash: ks[0].AttributeName}
	if len(ks) == 2 {
		if ks[1].KeyType != "RANGE" {
			return keySchema{}, invalidParam("Invalid KeySchema: The second KeySchemaElement is not a RANGE key type")
		}
		if ks[1].AttributeName == k.hash {
			return keySchema{}, invalidParam("Both the Hash Key and the Range Key element in the KeySchema have the same name")
		}
		k.rng = ks[1].AttributeName
	}
	return k, nil
}

func (k keySchema) wire() []KeySchemaElement {
	out := []KeySchemaElement{{k.hash, "HASH"}}
	if k.rng != "" {
		out = append(out, KeySchemaElement{k.rng, "RANGE"})
	}
	return out
}

func parseThroughput(pt *ProvisionedThroughput, billing, what string) (*throughput, error) {
	if billing == "PAY_PER_REQUEST" {
		if pt != nil && (pt.ReadCapacityUnits != nil && *pt.ReadCapacityUnits != 0 || pt.WriteCapacityUnits != nil && *pt.WriteCapacityUnits != 0) {
			return nil, invalidParam("Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		return nil, nil
	}
	if pt == nil {
		if what == "table" {
			return nil, validation("No provisioned throughput specified for the table")
		}
		return nil, invalidParam("ProvisionedThroughput must be specified for index: %s", what)
	}
	if pt.ReadCapacityUnits == nil || pt.WriteCapacityUnits == nil {
		return nil, invalidParam("ReadCapacityUnits and WriteCapacityUnits must both be specified when BillingMode is PROVISIONED")
	}
	if *pt.ReadCapacityUnits < 1 || *pt.WriteCapacityUnits < 1 {
		return nil, validation("1 validation error detected: Value '%d' at 'provisionedThroughput.readCapacityUnits' failed to satisfy constraint: Member must have value greater than or equal to 1", *pt.ReadCapacityUnits)
	}
	return &throughput{read: *pt.ReadCapacityUnits, write: *pt.WriteCapacityUnits}, nil
}

func parseProjection(p *Projection) (Projection, error) {
	if p == nil || p.ProjectionType == "" {
		return Projection{}, invalidParam("Unknown ProjectionType: null")
	}
	switch p.ProjectionType {
	case "ALL", "KEYS_ONLY":
		if len(p.NonKeyAttributes) > 0 {
			return Projection{}, invalidParam("ProjectionType is %s, but NonKeyAttributes is specified", p.ProjectionType)
		}
	case "INCLUDE":
		if len(p.NonKeyAttributes) == 0 {
			return Projection{}, invalidParam("ProjectionType is INCLUDE, but NonKeyAttributes is not specified")
		}
	default:
		return Projection{}, invalidParam("Unknown ProjectionType: %s", p.ProjectionType)
	}
	return Projection{p.ProjectionType, append([]string(nil), p.NonKeyAttributes...)}, nil
}

func (s *Service) newIndex(t *table, in indexInput, global bool, billing string) (*index, error) {
	if err := checkTableName(in.IndexName, "indexName"); err != nil {
		return nil, err
	}
	key, err := parseKeySchema(in.KeySchema, "keySchema")
	if err != nil {
		return nil, err
	}
	proj, err := parseProjection(in.Projection)
	if err != nil {
		return nil, err
	}
	ix := &index{name: in.IndexName, global: global, key: key, projection: proj, created: time.Now()}
	if !global {
		if t.key.rng == "" {
			return nil, invalidParam("Table KeySchema does not have a range key, which is required when specifying a LocalSecondaryIndex")
		}
		if key.hash != t.key.hash {
			return nil, invalidParam("Index KeySchema does not have the same leading hash key as table KeySchema for index: %s. index hash key: %s, table hash key: %s", in.IndexName, key.hash, t.key.hash)
		}
		if key.rng == "" {
			return nil, invalidParam("Index KeySchema: %s does not have a range key", in.IndexName)
		}
		return ix, nil
	}
	if ix.throughput, err = parseThroughput(in.ProvisionedThroughput, billing, in.IndexName); err != nil {
		return nil, err
	}
	return ix, nil
}

// checkAttributeDefinitions verifies every key attribute is defined and
// every definition is used.
func checkAttributeDefinitions(t *table) error {
	used := map[string]bool{}
	for _, n := range t.key.names() {
		used[n] = true
	}
	for _, ix := range t.indexes {
		for _, n := range ix.key.names() {
			used[n] = true
		}
	}
	for n := range used {
		if _, ok := t.attrTypes[n]; !ok {
			return invalidParam("Some index key attributes are not defined in AttributeDefinitions. Keys: [%s], AttributeDefinitions: [%s]", joinSorted(keysOf(used)), joinSorted(keysOf(t.attrTypes)))
		}
	}
	if len(used) != len(t.attrTypes) {
		return invalidParam("Number of attributes in KeySchema does not exactly match number of attributes defined in AttributeDefinitions")
	}
	return nil
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func parseAttrDefs(defs []AttributeDefinition, into map[string]string) error {
	for _, d := range defs {
		switch d.AttributeType {
		case "S", "N", "B":
		default:
			return validation("1 validation error detected: Value '%s' at 'attributeDefinitions.member.attributeType' failed to satisfy constraint: Member must satisfy enum value set: [B, N, S]", d.AttributeType)
		}
		if prev, ok := into[d.AttributeName]; ok && prev != d.AttributeType {
			return invalidParam("Cannot change the type of attribute %s", d.AttributeName)
		}
		into[d.AttributeName] = d.AttributeType
	}
	return nil
}

func (s *Service) createTable(r *http.Request, in *createTableInput) (interface{}, error) {
	if err := checkTableName(in.TableName, "tableName"); err != nil {
		return nil, err
	}
	billing := in.BillingMode
	if billing == "" {
		billing = "PROVISIONED"
	}
	if billing != "PROVISIONED" && billing != "PAY_PER_REQUEST" {
		return nil, validation("1 validation error detected: Value '%s' at 'billingMode' failed to satisfy constraint: Member must satisfy enum value set: [PROVISIONED, PAY_PER_REQUEST]", billing)
	}
	key, err := parseKeySchema(in.KeySchema, "keySchema")
	if err != nil {
		return nil, err
	}
	region := awsproto.ParseCredential(r).Region
	if region == "" {
		region = s.region
	}
	now := time.Now()
	t := &table{
		name: in.TableName, arn: awsproto.ARN("dynamodb", region, "table/"+in.TableName),
		id: awsproto.RequestID(), region: region, created: now, key: key,
		attrTypes: map[string]string{}, billing: billing, tags: map[string]string{},
		tableClass: in.TableClass, deletionProtection: in.DeletionProtectionEnabled,
		parts: map[string]*partition{},
	}
	if billing == "PAY_PER_REQUEST" {
		t.billingChanged = now
	}
	if err := parseAttrDefs(in.AttributeDefinitions, t.attrTypes); err != nil {
		return nil, err
	}
	if t.throughput, err = parseThroughput(in.ProvisionedThroughput, billing, "table"); err != nil {
		return nil, err
	}
	if len(in.LocalSecondaryIndexes) > 5 {
		return nil, validation("One or more parameter values were invalid: Number of LocalSecondaryIndexes exceeds per-table limit of 5")
	}
	if len(in.GlobalSecondaryIndexes) > 20 {
		return nil, ddbError("LimitExceededException", "Subscriber limit exceeded: The number of global secondary indexes exceeds the limit of 20")
	}
	seen := map[string]bool{}
	for i, list := range [][]indexInput{in.LocalSecondaryIndexes, in.GlobalSecondaryIndexes} {
		for _, ii := range list {
			ix, err := s.newIndex(t, ii, i == 1, billing)
			if err != nil {
				return nil, err
			}
			if seen[ix.name] {
				return nil, invalidParam("Duplicate index name: %s", ix.name)
			}
			seen[ix.name] = true
			t.indexes = append(t.indexes, ix)
		}
	}
	if err := checkAttributeDefinitions(t); err != nil {
		return nil, err
	}
	if err := t.setStream(in.StreamSpecification, now); err != nil {
		return nil, err
	}
	t.setSSE(in.SSESpecification)
	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}
	for _, tag := range in.Tags {
		t.tags[tag.Key] = tag.Value
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tables[t.name]; ok {
		return nil, ddbError("ResourceInUseException", "Table already exists: %s", t.name)
	}
	s.tables[t.name] = t
	return struct{ TableDescription tableDesc }{t.describe("ACTIVE")}, nil
}

func (t *table) setStream(spec *StreamSpecification, now time.Time) error {
	if spec == nil {
		return nil
	}
	if !spec.StreamEnabled {
		if spec.StreamViewType != "" {
			return validation("One or more parameter values were invalid: StreamViewType cannot be specified when StreamEnabled is false")
		}
		if t.stream != nil {
			t.stream = nil
		}
		return nil
	}
	switch spec.StreamViewType {
	case "NEW_IMAGE", "OLD_IMAGE", "NEW_AND_OLD_IMAGES", "KEYS_ONLY":
	default:
		return validation("One or more parameter values were invalid: StreamViewType must be specified when StreamEnabled is true")
	}
	if t.stream != nil {
		return validation("Table already has an enabled stream: %s", t.streamARN())
	}
	t.stream = &StreamSpecification{true, spec.StreamViewType}
	t.streamLabel = now.UTC().Format("2006-01-02T15:04:05.000")
	return nil
}

func (t *table) streamARN() string { return t.arn + "/stream/" + t.streamLabel }

func (t *table) setSSE(spec *SSESpecification) {
	if spec == nil {
		return
	}
	if spec.Enabled == nil || !*spec.Enabled {
		t.sse = nil
		return
	}
	c := *spec
	if c.SSEType == "" {
		c.SSEType = "KMS"
	}
	if c.KMSMasterKeyId == "" {
		c.KMSMasterKeyId = awsproto.ARN("kms", t.region, "key/"+awsproto.RequestID())
	} else if !strings.HasPrefix(c.KMSMasterKeyId, "arn:") {
		c.KMSMasterKeyId = awsproto.ARN("kms", t.region, "key/"+c.KMSMasterKeyId)
	}
	t.sse = &c
}

func (tp *throughput) desc() throughputDesc {
	if tp == nil {
		return throughputDesc{}
	}
	d := throughputDesc{ReadCapacityUnits: tp.read, WriteCapacityUnits: tp.write}
	if !tp.increased.IsZero() {
		f := nowFloat(tp.increased)
		d.LastIncreaseDateTime = &f
	}
	if !tp.decreased.IsZero() {
		f := nowFloat(tp.decreased)
		d.LastDecreaseDateTime = &f
	}
	return d
}

func (t *table) describe(status string) tableDesc {
	d := tableDesc{
		TableName: t.name, KeySchema: t.key.wire(), TableStatus: status,
		CreationDateTime: nowFloat(t.created), ProvisionedThroughput: t.throughput.desc(),
		TableSizeBytes: int64(t.size), ItemCount: int64(t.count), TableArn: t.arn, TableId: t.id,
		BillingModeSummary:        &billingSummary{BillingMode: t.billing},
		DeletionProtectionEnabled: t.deletionProtection,
		WarmThroughput:            &warmThroughput{12000, 4000, "ACTIVE"},
	}
	if t.billing == "PAY_PER_REQUEST" {
		f := nowFloat(t.billingChanged)
		d.BillingModeSummary.LastUpdateToPayPerRequestDateTime = &f
	}
	for _, n := range sortedKeysOf(t.attrTypes) {
		d.AttributeDefinitions = append(d.AttributeDefinitions, AttributeDefinition{n, t.attrTypes[n]})
	}
	for _, ix := range t.indexes {
		count, size := 0, 0
		for _, item := range t.rows(ix) {
			count++
			size += itemSize(t.projectIndex(ix, item))
		}
		x := indexDesc{IndexName: ix.name, KeySchema: ix.key.wire(), Projection: ix.projection,
			IndexSizeBytes: int64(size), ItemCount: int64(count), IndexArn: t.arn + "/index/" + ix.name}
		if ix.global {
			f := false
			tp := ix.throughput.desc()
			x.IndexStatus, x.Backfilling, x.ProvisionedThroughput = "ACTIVE", &f, &tp
			d.GlobalSecondaryIndexes = append(d.GlobalSecondaryIndexes, x)
		} else {
			d.LocalSecondaryIndexes = append(d.LocalSecondaryIndexes, x)
		}
	}
	if t.stream != nil {
		d.StreamSpecification = t.stream
		d.LatestStreamLabel = t.streamLabel
		d.LatestStreamArn = t.streamARN()
	}
	if t.sse != nil {
		d.SSEDescription = &sseDesc{Status: "ENABLED", SSEType: t.sse.SSEType, KMSMasterKeyArn: t.sse.KMSMasterKeyId}
	}
	if t.tableClass != "" {
		d.TableClassSummary = &struct{ TableClass string }{t.tableClass}
	}
	return d
}

func sortedKeysOf[T any](m map[string]T) []string {
	out := keysOf(m)
	sort.Strings(out)
	return out
}

func (s *Service) describeTable(_ *http.Request, in *tableNameInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	return struct{ Table tableDesc }{t.describe("ACTIVE")}, nil
}

func (s *Service) deleteTable(_ *http.Request, in *tableNameInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	if t.deletionProtection {
		return nil, validation("Resource cannot be deleted as it is currently protected against deletion. Disable deletion protection first.")
	}
	delete(s.tables, t.name)
	return struct{ TableDescription tableDesc }{t.describe("DELETING")}, nil
}

func (s *Service) updateTable(_ *http.Request, in *updateTableInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	if in.BillingMode == "" && in.ProvisionedThroughput == nil && len(in.GlobalSecondaryIndexUpdates) == 0 &&
		in.StreamSpecification == nil && in.SSESpecification == nil && in.TableClass == "" &&
		in.DeletionProtectionEnabled == nil && in.OnDemandThroughput == nil && len(in.ReplicaUpdates) == 0 {
		return nil, validation("At least one of ProvisionedThroughput, BillingMode, UpdateStreamEnabled, GlobalSecondaryIndexUpdates or SSESpecification or ReplicaUpdates is required")
	}
	// Work on a copy so a failed update changes nothing.
	c := *t
	c.attrTypes = map[string]string{}
	for k, v := range t.attrTypes {
		c.attrTypes[k] = v
	}
	c.indexes = append([]*index(nil), t.indexes...)
	now := time.Now()

	if err := parseAttrDefs(in.AttributeDefinitions, c.attrTypes); err != nil {
		return nil, err
	}
	if in.BillingMode != "" && in.BillingMode != c.billing {
		if in.BillingMode != "PROVISIONED" && in.BillingMode != "PAY_PER_REQUEST" {
			return nil, validation("1 validation error detected: Value '%s' at 'billingMode' failed to satisfy constraint: Member must satisfy enum value set: [PROVISIONED, PAY_PER_REQUEST]", in.BillingMode)
		}
		c.billing = in.BillingMode
		if c.billing == "PAY_PER_REQUEST" {
			c.billingChanged, c.throughput = now, nil
			for i, ix := range c.indexes {
				if ix.global {
					cp := *ix
					cp.throughput = nil
					c.indexes[i] = &cp
				}
			}
		} else if in.ProvisionedThroughput == nil {
			return nil, validation("One or more parameter values were invalid: ProvisionedThroughput must be specified when BillingMode is PROVISIONED")
		}
	}
	if in.ProvisionedThroughput != nil {
		tp, err := parseThroughput(in.ProvisionedThroughput, c.billing, "table")
		if err != nil {
			return nil, err
		}
		c.throughput = changeThroughput(t.throughput, tp, now)
	}
	for _, u := range in.GlobalSecondaryIndexUpdates {
		switch {
		case u.Create != nil:
			ix, err := s.newIndex(&c, *u.Create, true, c.billing)
			if err != nil {
				return nil, err
			}
			if _, err := c.index(ix.name); err == nil {
				return nil, invalidParam("Duplicate index name: %s", ix.name)
			}
			c.indexes = append(c.indexes, ix)
		case u.Update != nil:
			ix, err := c.index(u.Update.IndexName)
			if err != nil || !ix.global {
				return nil, ddbError("ResourceNotFoundException", "Requested resource not found: Index: %s not found", u.Update.IndexName)
			}
			if u.Update.ProvisionedThroughput != nil {
				tp, err := parseThroughput(u.Update.ProvisionedThroughput, c.billing, ix.name)
				if err != nil {
					return nil, err
				}
				cp := *ix
				cp.throughput = changeThroughput(ix.throughput, tp, now)
				replaceIndex(&c, &cp)
			}
		case u.Delete != nil:
			ix, err := c.index(u.Delete.IndexName)
			if err != nil || !ix.global {
				return nil, ddbError("ResourceNotFoundException", "Requested resource not found: Index: %s not found", u.Delete.IndexName)
			}
			kept := c.indexes[:0:0]
			for _, x := range c.indexes {
				if x != ix {
					kept = append(kept, x)
				}
			}
			c.indexes = kept
		}
	}
	if len(in.GlobalSecondaryIndexUpdates) > 0 || len(in.AttributeDefinitions) > 0 {
		// Definitions only used by deleted indexes are dropped.
		used := map[string]bool{}
		for _, n := range c.key.names() {
			used[n] = true
		}
		for _, ix := range c.indexes {
			for _, n := range ix.key.names() {
				used[n] = true
			}
		}
		for n := range c.attrTypes {
			if !used[n] {
				delete(c.attrTypes, n)
			}
		}
		if err := checkAttributeDefinitions(&c); err != nil {
			return nil, err
		}
	}
	if in.StreamSpecification != nil {
		if !in.StreamSpecification.StreamEnabled && c.stream == nil {
			return nil, validation("Table already has no stream enabled")
		}
		if err := c.setStream(in.StreamSpecification, now); err != nil {
			return nil, err
		}
	}
	c.setSSE(in.SSESpecification)
	if in.TableClass != "" {
		c.tableClass = in.TableClass
	}
	if in.DeletionProtectionEnabled != nil {
		c.deletionProtection = *in.DeletionProtectionEnabled
	}
	*t = c
	return struct{ TableDescription tableDesc }{t.describe("ACTIVE")}, nil
}

func replaceIndex(t *table, ix *index) {
	for i, x := range t.indexes {
		if x.name == ix.name {
			t.indexes[i] = ix
		}
	}
}

func changeThroughput(old, tp *throughput, now time.Time) *throughput {
	if tp == nil {
		return nil
	}
	if old != nil {
		tp.increased, tp.decreased = old.increased, old.decreased
		if tp.read > old.read || tp.write > old.write {
			tp.increased = now
		}
		if tp.read < old.read || tp.write < old.write {
			tp.decreased = now
		}
	}
	return tp
}

type listTablesInput struct {
	ExclusiveStartTableName string
	Limit                   *int
}

func (s *Service) listTables(_ *http.Request, in *listTablesInput) (interface{}, error) {
	limit := 100
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 100 {
			return nil, validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100", *in.Limit)
		}
		limit = *in.Limit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := struct {
		TableNames             []string
		LastEvaluatedTableName string `json:",omitempty"`
	}{TableNames: []string{}}
	names := sortedKeysOf(s.tables)
	for i, n := range names {
		if n <= in.ExclusiveStartTableName {
			continue
		}
		if len(out.TableNames) == limit {
			out.LastEvaluatedTableName = names[i-1]
			break
		}
		out.TableNames = append(out.TableNames, n)
	}
	return out, nil
}

// Tags.

type tagInput struct {
	ResourceArn string
	Tags        []Tag
	TagKeys     []string
	NextToken   string
}

func checkTags(tags []Tag) error {
	if len(tags) > 50 {
		return validation("One or more parameter values were invalid: Too many tags")
	}
	for _, t := range tags {
		if t.Key == "" || len(t.Key) > 128 || len(t.Value) > 256 {
			return validation("One or more parameter values were invalid: Tag key or value is invalid")
		}
	}
	return nil
}

func (s *Service) byARN(arn string) (*table, error) {
	if arn == "" {
		return nil, validation("1 validation error detected: Value null at 'resourceArn' failed to satisfy constraint: Member must not be null")
	}
	for _, t := range s.tables {
		if t.arn == arn {
			return t, nil
		}
	}
	if !strings.HasPrefix(arn, "arn:aws:dynamodb:") {
		return nil, validation("Invalid TableArn: Invalid ResourceArn provided as input %s", arn)
	}
	return nil, ddbError("ResourceNotFoundException", "Requested resource not found: ResourcArn: %s not found", arn)
}

func (s *Service) tagResource(_ *http.Request, in *tagInput) (interface{}, error) {
	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.byARN(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	for _, tag := range in.Tags {
		t.tags[tag.Key] = tag.Value
	}
	if len(t.tags) > 50 {
		return nil, validation("One or more parameter values were invalid: Too many tags")
	}
	return nil, nil
}

func (s *Service) untagResource(_ *http.Request, in *tagInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.byARN(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	for _, k := range in.TagKeys {
		delete(t.tags, k)
	}
	return nil, nil
}

func (s *Service) listTags(_ *http.Request, in *tagInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.byARN(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	out := struct{ Tags []Tag }{Tags: []Tag{}}
	for _, k := range sortedKeysOf(t.tags) {
		out.Tags = append(out.Tags, Tag{k, t.tags[k]})
	}
	return out, nil
}

// Time to live. Expired items are not deleted: DynamoDB only promises to
// delete them eventually, and tests rarely want them to vanish.

type ttlInput struct {
	TableName               string
	TimeToLiveSpecification *TimeToLiveSpecification
}

func (s *Service) updateTTL(_ *http.Request, in *ttlInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	spec := in.TimeToLiveSpecification
	if spec == nil || spec.AttributeName == "" {
		return nil, validation("1 validation error detected: Value null at 'timeToLiveSpecification.attributeName' failed to satisfy constraint: Member must not be null")
	}
	enabled := t.ttl != nil && t.ttl.Enabled
	if spec.Enabled && enabled {
		return nil, validation("TimeToLive is already enabled")
	}
	if !spec.Enabled && !enabled {
		return nil, validation("TimeToLive is already disabled")
	}
	if !spec.Enabled && spec.AttributeName != t.ttl.AttributeName {
		return nil, validation("TimeToLive attribute name does not match the current attribute name")
	}
	c := *spec
	t.ttl = &c
	return struct{ TimeToLiveSpecification TimeToLiveSpecification }{c}, nil
}

func (s *Service) describeTTL(_ *http.Request, in *tableNameInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	type desc struct {
		TimeToLiveStatus string
		AttributeName    string `json:",omitempty"`
	}
	d := desc{TimeToLiveStatus: "DISABLED"}
	if t.ttl != nil && t.ttl.Enabled {
		d = desc{"ENABLED", t.ttl.AttributeName}
	}
	return struct{ TimeToLiveDescription desc }{d}, nil
}

// Point-in-time recovery: the flag is stored; no backups are kept.

type backupsInput struct {
	TableName                        string
	PointInTimeRecoverySpecification *struct {
		PointInTimeRecoveryEnabled *bool
		RecoveryPeriodInDays       *int
	}
}

func (s *Service) backupsDesc(t *table) interface{} {
	type pitrDesc struct {
		PointInTimeRecoveryStatus  string
		RecoveryPeriodInDays       int      `json:",omitempty"`
		EarliestRestorableDateTime *float64 `json:",omitempty"`
		LatestRestorableDateTime   *float64 `json:",omitempty"`
	}
	p := pitrDesc{PointInTimeRecoveryStatus: "DISABLED"}
	if t.pitr {
		earliest, latest := nowFloat(t.created), nowFloat(time.Now())
		p = pitrDesc{"ENABLED", 35, &earliest, &latest}
	}
	return struct {
		ContinuousBackupsDescription struct {
			ContinuousBackupsStatus        string
			PointInTimeRecoveryDescription pitrDesc
		}
	}{struct {
		ContinuousBackupsStatus        string
		PointInTimeRecoveryDescription pitrDesc
	}{"ENABLED", p}}
}

func (s *Service) describeBackups(_ *http.Request, in *tableNameInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	return s.backupsDesc(t), nil
}

func (s *Service) updateBackups(_ *http.Request, in *backupsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	spec := in.PointInTimeRecoverySpecification
	if spec == nil || spec.PointInTimeRecoveryEnabled == nil {
		return nil, validation("1 validation error detected: Value null at 'pointInTimeRecoverySpecification.pointInTimeRecoveryEnabled' failed to satisfy constraint: Member must not be null")
	}
	t.pitr = *spec.PointInTimeRecoveryEnabled
	return s.backupsDesc(t), nil
}

func (s *Service) describeLimits(_ *http.Request, _ *struct{}) (interface{}, error) {
	return map[string]int{
		"AccountMaxReadCapacityUnits": 80000, "AccountMaxWriteCapacityUnits": 80000,
		"TableMaxReadCapacityUnits": 40000, "TableMaxWriteCapacityUnits": 40000,
	}, nil
}

func (s *Service) describeEndpoints(r *http.Request, _ *struct{}) (interface{}, error) {
	type endpoint struct {
		Address              string
		CachePeriodInMinutes int
	}
	return struct{ Endpoints []endpoint }{[]endpoint{{r.Host, 1440}}}, nil
}
