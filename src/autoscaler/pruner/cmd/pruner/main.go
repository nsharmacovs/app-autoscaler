package main

import (
	"autoscaler/db"
	"autoscaler/db/sqldb"
	"autoscaler/helpers"
	"autoscaler/pruner"
	"autoscaler/pruner/config"
	sync "autoscaler/sync"
	"flag"
	"fmt"
	"os"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"
)

func main() {
	var path string
	flag.StringVar(&path, "c", "", "config file")
	flag.Parse()
	if path == "" {
		fmt.Fprintln(os.Stderr, "missing config file")
		os.Exit(1)
	}

	configFile, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stdout, "failed to open config file '%s' : %s\n", path, err.Error())
		os.Exit(1)
	}

	var conf *config.Config
	conf, err = config.LoadConfig(configFile)
	if err != nil {
		fmt.Fprintf(os.Stdout, "failed to read config file '%s' : %s\n", path, err.Error())
		os.Exit(1)
	}
	configFile.Close()

	err = conf.Validate()
	if err != nil {
		fmt.Fprintf(os.Stdout, "failed to validate configuration : %s\n", err.Error())
		os.Exit(1)
	}

	logger := initLoggerFromConfig(&conf.Logging)
	prClock := clock.NewClock()

	instanceMetricsDb, err := sqldb.NewInstanceMetricsSQLDB(conf.InstanceMetricsDb.Db, logger.Session("instancemetrics-db"))
	if err != nil {
		logger.Error("failed to connect instancemetrics db", err, lager.Data{"dbConfig": conf.InstanceMetricsDb.Db})
		os.Exit(1)
	}
	defer instanceMetricsDb.Close()

	appMetricsDb, err := sqldb.NewAppMetricSQLDB(conf.AppMetricsDb.Db, logger.Session("appmetrics-db"))
	if err != nil {
		logger.Error("failed to connect appmetrics db", err, lager.Data{"dbConfig": conf.AppMetricsDb.Db})
		os.Exit(1)
	}
	defer appMetricsDb.Close()

	scalingEngineDb, err := sqldb.NewScalingEngineSQLDB(conf.ScalingEngineDb.Db, logger.Session("scalingengine-db"))
	if err != nil {
		logger.Error("failed to connect scalingengine db", err, lager.Data{"dbConfig": conf.ScalingEngineDb.Db})
		os.Exit(1)
	}
	defer scalingEngineDb.Close()

	prunerLoggerSessionName := "instancemetrics-dbpruner"
	instanceMetricDbPruner := pruner.NewInstanceMetricsDbPruner(instanceMetricsDb, conf.InstanceMetricsDb.CutoffDays, prClock, logger.Session(prunerLoggerSessionName))
	instanceMetricsDbPrunerRunner := pruner.NewDbPrunerRunner(instanceMetricDbPruner, conf.InstanceMetricsDb.RefreshInterval, prClock, logger.Session(prunerLoggerSessionName))

	prunerLoggerSessionName = "appmetrics-dbpruner"
	appMetricsDbPruner := pruner.NewAppMetricsDbPruner(appMetricsDb, conf.AppMetricsDb.CutoffDays, prClock, logger.Session(prunerLoggerSessionName))
	appMetricsDbPrunerRunner := pruner.NewDbPrunerRunner(appMetricsDbPruner, conf.AppMetricsDb.RefreshInterval, prClock, logger.Session(prunerLoggerSessionName))

	prunerLoggerSessionName = "scalingengine-dbpruner"
	scalingEngineDbPruner := pruner.NewScalingEngineDbPruner(scalingEngineDb, conf.ScalingEngineDb.CutoffDays, prClock, logger.Session(prunerLoggerSessionName))
	scalingEngineDbPrunerRunner := pruner.NewDbPrunerRunner(scalingEngineDbPruner, conf.ScalingEngineDb.RefreshInterval, prClock, logger.Session(prunerLoggerSessionName))

	members := grouper.Members{
		{"instancemetrics-dbpruner", instanceMetricsDbPrunerRunner},
		{"appmetrics-dbpruner", appMetricsDbPrunerRunner},
		{"scalingEngine-dbpruner", scalingEngineDbPrunerRunner},
	}

	guid, err := helpers.GenerateGUID(logger)
	if err != nil {
		logger.Error("failed-to-generate-guid", err)
	}
	const lockTableName = "pruner_lock"
	if conf.EnableDBLock {
		logger.Debug("database-lock-feature-enabled")
		var lockDB db.LockDB
		lockDB, err = sqldb.NewLockSQLDB(conf.DBLock.LockDB, lockTableName, logger.Session("lock-db"))
		if err != nil {
			logger.Error("failed-to-connect-lock-database", err, lager.Data{"dbConfig": conf.DBLock.LockDB})
			os.Exit(1)
		}
		defer lockDB.Close()
		prdl := sync.NewDatabaseLock(logger)
		dbLockMaintainer := prdl.InitDBLockRunner(conf.DBLock.LockRetryInterval, conf.DBLock.LockTTL, guid, lockDB)
		members = append(grouper.Members{{"db-lock-maintainer", dbLockMaintainer}}, members...)
	}

	if conf.Lock.ConsulClusterConfig != "" {
		consulClient, err := consuladapter.NewClientFromUrl(conf.Lock.ConsulClusterConfig)
		if err != nil {
			logger.Fatal("new consul client failed", err)
		}

		serviceClient := pruner.NewServiceClient(consulClient, prClock)

		guid, err := helpers.GenerateGUID(logger)
		if err != nil {
			logger.Error("failed-to-generate-guid", err)
			os.Exit(1)
		}
		if !conf.EnableDBLock {
			lockMaintainer := serviceClient.NewPrunerLockRunner(
				logger,
				guid,
				conf.Lock.LockRetryInterval,
				conf.Lock.LockTTL,
			)
			members = append(grouper.Members{{"lock-maintainer", lockMaintainer}}, members...)
		}
	}

	monitor := ifrit.Invoke(sigmon.New(grouper.NewOrdered(os.Interrupt, members)))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")

}

func initLoggerFromConfig(conf *config.LoggingConfig) lager.Logger {
	logLevel, err := getLogLevel(conf.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %s\n", err.Error())
		os.Exit(1)
	}
	logger := lager.NewLogger("pruner")

	keyPatterns := []string{"[Pp]wd", "[Pp]ass", "[Ss]ecret", "[Tt]oken"}

	redactedSink, err := helpers.NewRedactingWriterWithURLCredSink(os.Stdout, logLevel, keyPatterns, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create redacted sink", err.Error())
	}
	logger.RegisterSink(redactedSink)

	return logger
}

func getLogLevel(level string) (lager.LogLevel, error) {
	switch level {
	case "debug":
		return lager.DEBUG, nil
	case "info":
		return lager.INFO, nil
	case "error":
		return lager.ERROR, nil
	case "fatal":
		return lager.FATAL, nil
	default:
		return -1, fmt.Errorf("Error: unsupported log level:%s", level)
	}
}
