package rds

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

// The engines may runs, with a few versions of each major line. In real
// mode a version runs in the official image for its major line
// (postgres:16, mysql:8.0, mariadb:10.11), whatever the minor version.
type engine struct {
	name, description string
	port              int
	license           string
	logTypes          []string
	versions          []engineVersion // ascending
}

type engineVersion struct {
	version, major, family string
	isDefault              bool
}

var engines = map[string]*engine{
	"postgres": {name: "postgres", description: "PostgreSQL", port: 5432, license: "postgresql-license",
		logTypes: []string{"postgresql", "upgrade"},
		versions: versions("postgres", "13.20 14.17 15.12 16.8 *17.4")},
	"mysql": {name: "mysql", description: "MySQL Community Edition", port: 3306, license: "general-public-license",
		logTypes: []string{"audit", "error", "general", "slowquery"},
		versions: versions("mysql", "8.0.41 *8.4.4")},
	"mariadb": {name: "mariadb", description: "MariaDB Community Edition", port: 3306, license: "general-public-license",
		logTypes: []string{"audit", "error", "general", "slowquery"},
		versions: versions("mariadb", "10.6.21 10.11.11 *11.4.5")},
}

func versions(name, list string) []engineVersion {
	var out []engineVersion
	for _, v := range strings.Fields(list) {
		def := strings.HasPrefix(v, "*")
		v = strings.TrimPrefix(v, "*")
		major := majorOf(name, v)
		out = append(out, engineVersion{version: v, major: major, family: name + major, isDefault: def})
	}
	return out
}

// majorOf returns the major line of a version: "16" for PostgreSQL 16.8,
// "8.0" for MySQL 8.0.41.
func majorOf(engine, v string) string {
	parts := strings.Split(v, ".")
	if engine == "postgres" || len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + "." + parts[1]
}

func lookupEngine(name string) (*engine, error) {
	e, ok := engines[name]
	if !ok {
		return nil, errInvalid("Invalid DB engine: %s (may supports postgres, mysql and mariadb)", name)
	}
	return e, nil
}

func (e *engine) defaultVersion() engineVersion {
	for _, v := range e.versions {
		if v.isDefault {
			return v
		}
	}
	return e.versions[len(e.versions)-1]
}

// resolve picks the version for EngineVersion: the default when empty, the
// newest of a major line ("16"), or an exact version. Unlisted minor
// versions of a known major line are accepted as given.
func (e *engine) resolve(v string) (engineVersion, error) {
	if v == "" {
		return e.defaultVersion(), nil
	}
	var newest *engineVersion
	for i, ev := range e.versions {
		if ev.version == v {
			return ev, nil
		}
		if ev.major == v {
			newest = &e.versions[i]
		}
	}
	if newest != nil {
		return *newest, nil
	}
	major := majorOf(e.name, v)
	for _, ev := range e.versions {
		if ev.major == major && strings.HasPrefix(v, major+".") {
			return engineVersion{version: v, major: major, family: ev.family}, nil
		}
	}
	return engineVersion{}, errCombination("Cannot find version %s for %s", v, e.name)
}

func (e *engine) image(v engineVersion) string { return e.name + ":" + v.major }

// familyEngine returns the engine of a parameter group family.
func familyEngine(family string) (*engine, bool) {
	for _, e := range engines {
		for _, v := range e.versions {
			if v.family == family {
				return e, true
			}
		}
	}
	return nil, false
}

// Instance classes: db.<family>.<size>. The size sets the vCPUs and memory
// a real-mode container may use; memory-optimized families get twice the
// memory.
var classRE = regexp.MustCompile(`^db\.([a-z][a-z0-9-]*)\.(micro|small|medium|large|xlarge|(\d+)xlarge)$`)

type instanceClass struct{ vcpus, memMiB int }

func parseClass(c string) (instanceClass, bool) {
	m := classRE.FindStringSubmatch(c)
	if m == nil {
		return instanceClass{}, false
	}
	var ic instanceClass
	switch m[2] {
	case "micro":
		ic = instanceClass{2, 1024}
	case "small":
		ic = instanceClass{2, 2048}
	case "medium":
		ic = instanceClass{2, 4096}
	case "large":
		ic = instanceClass{2, 8192}
	case "xlarge":
		ic = instanceClass{4, 16384}
	default:
		n, _ := strconv.Atoi(m[3])
		ic = instanceClass{4 * n, 16384 * n}
	}
	if strings.HasPrefix(m[1], "r") || strings.HasPrefix(m[1], "x") || strings.HasPrefix(m[1], "z") {
		if m[2] != "micro" && m[2] != "small" && m[2] != "medium" {
			ic.memMiB *= 2
		}
	}
	return ic, true
}

// DescribeDBEngineVersions.

type xEngineVersion struct {
	Engine                             string
	MajorEngineVersion                 string
	EngineVersion                      string
	DBParameterGroupFamily             string
	DBEngineDescription                string
	DBEngineVersionDescription         string
	ExportableLogTypes                 awsproto.Members[string]
	SupportsLogExportsToCloudwatchLogs bool
	SupportsReadReplica                bool
	SupportedEngineModes               awsproto.Members[string]
	Status                             string
	ValidUpgradeTarget                 struct{}
	SupportedFeatureNames              awsproto.Members[string]
	TagList                            xTagList
}

func (s *Service) registerEngines() {
	s.handle("DescribeDBEngineVersions", s.describeEngineVersions)
}

func (s *Service) describeEngineVersions(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, version, family := f.Get("Engine"), f.Get("EngineVersion"), f.Get("DBParameterGroupFamily")
	if name != "" {
		if _, err := lookupEngine(name); err != nil {
			return nil, err
		}
	}
	filters := parseFilters(f)
	var out []xEngineVersion
	for _, en := range []string{"mariadb", "mysql", "postgres"} {
		e := engines[en]
		if name != "" && name != en {
			continue
		}
		for _, v := range e.versions {
			if version != "" && v.version != version && v.major != version {
				continue
			}
			if family != "" && v.family != family {
				continue
			}
			if f.Bool("DefaultOnly", false) && !v.isDefault {
				continue
			}
			ok, err := matches(filters, map[string][]string{"engine": {e.name}, "engine-version": {v.version},
				"db-parameter-group-family": {v.family}, "engine-mode": {"provisioned"}, "status": {"available"}})
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			out = append(out, xEngineVersion{
				Engine: e.name, MajorEngineVersion: v.major, EngineVersion: v.version, DBParameterGroupFamily: v.family,
				DBEngineDescription: e.description, DBEngineVersionDescription: e.description + " " + v.version,
				ExportableLogTypes: awsproto.List(e.logTypes), SupportsLogExportsToCloudwatchLogs: true,
				SupportsReadReplica: true, SupportedEngineModes: awsproto.List([]string{"provisioned"}), Status: "available",
				SupportedFeatureNames: awsproto.List([]string{}),
			})
		}
	}
	page, marker, err := paginate(out, f)
	if err != nil {
		return nil, err
	}
	return struct {
		DBEngineVersions struct{ DBEngineVersion []xEngineVersion }
		Marker           string `xml:",omitempty"`
	}{struct{ DBEngineVersion []xEngineVersion }{page}, marker}, nil
}
