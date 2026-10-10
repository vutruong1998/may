// Command may runs a local emulator compatible with the AWS APIs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/maycloud/may/internal/cloudwatch"
	"github.com/maycloud/may/internal/docker"
	"github.com/maycloud/may/internal/dynamodb"
	"github.com/maycloud/may/internal/ec2"
	"github.com/maycloud/may/internal/iam"
	"github.com/maycloud/may/internal/persist"
	"github.com/maycloud/may/internal/rds"
	"github.com/maycloud/may/internal/router"
	"github.com/maycloud/may/internal/s3"
	"github.com/maycloud/may/internal/sqs"
	"github.com/maycloud/may/internal/sts"
)

var version = "0.1.0-dev"

// saveInterval is how often -data-dir state is saved when something changed.
const saveInterval = 5 * time.Second

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := flag.Int("port", envInt("MAY_PORT", 4566), "port to listen on (env MAY_PORT)")
	host := flag.String("host", env("MAY_HOST", "0.0.0.0"), "address to bind (env MAY_HOST)")
	region := flag.String("region", env("MAY_REGION", "us-east-1"), "default region (env MAY_REGION)")
	hostnames := flag.String("hostname", env("MAY_HOSTNAME", ""), "extra comma-separated hostnames for virtual-host-style S3 (env MAY_HOSTNAME)")
	quiet := flag.Bool("quiet", os.Getenv("MAY_QUIET") != "", "don't log each request (env MAY_QUIET)")
	dataDir := flag.String("data-dir", env("MAY_DATA_DIR", ""), "keep state in this directory across restarts; empty means in memory only (env MAY_DATA_DIR)")
	useDocker := flag.Bool("docker", os.Getenv("MAY_DOCKER") != "", "run EC2 instances and RDS databases as Docker containers (env MAY_DOCKER)")
	healthcheck := flag.Bool("healthcheck", false, "query a running server's health endpoint and exit (for Docker HEALTHCHECK)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("may", version)
		return
	}
	if *healthcheck {
		os.Exit(checkHealth(*port))
	}

	var extra []string
	for _, h := range strings.Split(*hostnames, ",") {
		if h = strings.TrimSpace(h); h != "" {
			extra = append(extra, strings.ToLower(h))
		}
	}

	iamSvc := iam.New()
	stsSvc := sts.New(iamSvc)
	rt := router.New(version)
	rt.Log = !*quiet
	rt.Register("iam", iamSvc)
	rt.Register("sts", stsSvc, sts.Actions...)
	s3Svc := s3.New(*region, extra...)
	rt.Register("s3", s3Svc)
	sqsSvc := sqs.New(*region) // fills sqs.Actions
	rt.Register("sqs", sqsSvc, sqs.Actions...)
	rt.Target("AmazonSQS", "sqs")
	ddbSvc := dynamodb.New(*region)
	rt.Register("dynamodb", ddbSvc)
	rt.Target("DynamoDB_20120810", "dynamodb")
	logsSvc := cloudwatch.New(*region)
	rt.Register("logs", logsSvc)
	ec2Svc := ec2.New(*region) // fills ec2.Actions
	ec2Svc.SetIAM(iamSvc)
	ec2Svc.SetSTS(stsCredentials{stsSvc})
	rt.Register("ec2", ec2Svc, ec2.Actions...)
	rt.Internal("/imds/", http.HandlerFunc(ec2Svc.ServeIMDS))
	rdsSvc := rds.New(*region, rdsNetwork{ec2Svc}) // fills rds.Actions
	rt.Register("rds", rdsSvc, rds.Actions...)
	var store *persist.Store
	if *dataDir != "" {
		var err error
		if store, err = persist.Open(*dataDir); err != nil {
			log.Fatalf("-data-dir: %v", err)
		}
		for _, n := range []struct {
			name string
			svc  persist.Service
		}{{"iam", iamSvc}, {"sts", stsSvc}, {"s3", s3Svc}, {"sqs", sqsSvc}, {"dynamodb", ddbSvc},
			{"logs", logsSvc}, {"ec2", ec2Svc}, {"rds", rdsSvc}} {
			store.Add(n.name, n.svc)
		}
		loaded, err := store.Load()
		if err != nil {
			log.Fatalf("-data-dir: %v (move the file away to start without it)", err)
		}
		if len(loaded) > 0 {
			log.Printf("loaded state from %s (%s)", *dataDir, strings.Join(loaded, ", "))
		} else {
			log.Printf("keeping state in %s", *dataDir)
		}
		rt.Changed = store.Touch
	}
	var dockerDriver *ec2.DockerDriver
	var rdsDriver *rds.DockerDriver
	if *useDocker {
		dc, err := docker.New()
		if err != nil {
			log.Fatalf("-docker: %v", err)
		}
		dockerDriver = ec2.NewDockerDriver(ec2Svc, dc, *port, store != nil)
		ec2Svc.SetDriver(dockerDriver)
		log.Printf("EC2 real mode: instances run as containers (Docker API %s, %d CPUs)", dc.Version, dc.NCPU)
		rt.Internal("/ec2/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(dockerDriver.Endpoints())
		}))
		rdsDriver = rds.NewDockerDriver(rdsSvc, dc, *port, *region, store != nil)
		rdsSvc.SetDriver(rdsDriver)
		log.Printf("RDS real mode: DB instances run as postgres, mysql and mariadb containers")
		rt.Internal("/rds/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rdsDriver.Endpoints())
		}))
		// Instances and databases loaded from -data-dir get their
		// containers back.
		ec2Svc.Resume()
		rdsSvc.Resume()
	}
	rt.Target("Logs_20140328", "logs")
	rt.Fallback("s3")

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	srv := &http.Server{Addr: addr, Handler: rt, ReadHeaderTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutDown := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		close(shutDown)
	}()
	if store != nil {
		go store.Run(ctx, saveInterval)
	}

	log.Printf("may %s listening on %s (region %s; services: dynamodb, ec2, iam, logs, rds, s3, sqs, sts)", version, addr, *region)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutDown // let requests in progress finish
		if store != nil {
			if serr := store.Save(true); serr != nil {
				log.Printf("saving state: %v", serr)
			} else {
				log.Printf("state saved to %s", *dataDir)
			}
		}
	}
	if dockerDriver != nil {
		if store != nil {
			log.Printf("stopping instance and database containers (kept for the next start)")
		} else {
			log.Printf("removing instance and database containers")
		}
		dockerDriver.Close() // without -data-dir, instances don't outlive may's in-memory state
		rdsDriver.Close()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// stsCredentials adapts STS to the EC2 metadata service.
type stsCredentials struct{ s *sts.Service }

func (c stsCredentials) IssueRoleCredentials(roleARN, session string, d time.Duration) (string, string, string, time.Time, bool) {
	rc, ok := c.s.AssumeRoleFor(roleARN, session, d)
	return rc.AccessKeyID, rc.SecretAccessKey, rc.SessionToken, rc.Expiration, ok
}

// rdsNetwork adapts EC2 subnets and security groups to RDS.
type rdsNetwork struct{ ec2 *ec2.Service }

func (n rdsNetwork) LookupSubnet(region, id string) (rds.Subnet, bool) {
	sn, ok := n.ec2.LookupSubnet(region, id)
	return rds.Subnet(sn), ok
}

func (n rdsNetwork) DefaultSubnets(region string) []rds.Subnet {
	var out []rds.Subnet
	for _, sn := range n.ec2.DefaultSubnets(region) {
		out = append(out, rds.Subnet(sn))
	}
	return out
}

func (n rdsNetwork) LookupSecurityGroup(region, id string) (string, bool) {
	return n.ec2.LookupSecurityGroup(region, id)
}

func (n rdsNetwork) DefaultSecurityGroup(region, vpcID string) string {
	return n.ec2.DefaultSecurityGroup(region, vpcID)
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

func checkHealth(port int) int {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/_may/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
