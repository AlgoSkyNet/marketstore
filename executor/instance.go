package executor

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/alpacahq/marketstore/catalog"
	"github.com/alpacahq/marketstore/plugins/trigger"
	"github.com/alpacahq/marketstore/utils"
	"github.com/alpacahq/marketstore/utils/io"
	. "github.com/alpacahq/marketstore/utils/log"
	"github.com/golang/glog"
)

var ThisInstance *InstanceMetadata

type InstanceMetadata struct {
	InstanceID      int64
	RootDir         string
	CatalogDir      *catalog.Directory
	MetadataDBpath  string
	TXNPipe         *TransactionPipe
	WALFile         *WALFileType
	AggregateCache  AggCache
	WALWg           sync.WaitGroup
	ShutdownPending bool
	WALBypass       bool
	TriggerMatchers []*trigger.TriggerMatcher
}

func NewInstanceSetup(relRootDir string, options ...bool) {
	/*
		Defaults
	*/
	initCatalog, initWALCache, backgroundSync, WALBypass := true, true, true, false
	switch {
	case len(options) >= 4:
		WALBypass = options[3]
		fallthrough
	case len(options) == 3:
		backgroundSync = options[2]
		fallthrough
	case len(options) == 2:
		initWALCache = options[1]
		fallthrough
	case len(options) == 1:
		initCatalog = options[0]
	}
	Log(INFO, "WAL Setup: initCatalog %v, initWALCache %v, backgroundSync %v, WALBypass %v: \n",
		initCatalog, initWALCache, backgroundSync, WALBypass)

	if ThisInstance == nil {
		ThisInstance = new(InstanceMetadata)
	}
	var err error
	Log(INFO, "Root Directory: %s", relRootDir)
	rootDir, err := filepath.Abs(filepath.Clean(relRootDir))
	if err != nil {
		Log(ERROR, "Can not take absolute path of root directory %s", err.Error())
	}
	ThisInstance.InstanceID = time.Now().UTC().UnixNano()
	ThisInstance.RootDir = rootDir
	ThisInstance.MetadataDBpath = filepath.Join(rootDir, "metadata.db")
	ThisInstance.AggregateCache.DataMap = make(map[io.TimeBucketKey][]byte)
	// Initialize a global catalog
	if initCatalog {
		ThisInstance.CatalogDir = catalog.NewDirectory(rootDir)
	}
	ThisInstance.WALBypass = WALBypass
	if initWALCache {
		// Allocate a new WALFile and cache
		if WALBypass {
			ThisInstance.TXNPipe = NewTransactionPipe()
			ThisInstance.WALFile = &WALFileType{RootPath: ThisInstance.RootDir}
		} else {
			ThisInstance.TXNPipe, ThisInstance.WALFile, err = StartupCacheAndWAL(ThisInstance.RootDir)
			if err != nil {
				Log(FATAL, "Unable to startup Cache and WAL")
			}
		}
		if backgroundSync {
			// Startup the WAL and Primary cache flushers
			go ThisInstance.WALFile.SyncWAL(500*time.Millisecond, 5*time.Minute, utils.InstanceConfig.WALRotateInterval)
			ThisInstance.WALWg.Add(1)
		}
	}

	InitializeTriggers()
}

func InitializeTriggers() {
	glog.Info("InitializeTriggers")
	config := utils.InstanceConfig
	for _, triggerSetting := range config.Triggers {
		glog.Infof("triggerSetting = %v", triggerSetting)
		tmatcher := triggerSetting.NewInstance()
		ThisInstance.TriggerMatchers = append(ThisInstance.TriggerMatchers, tmatcher)
	}
}

type AggCache struct {
	sync.RWMutex
	DataMap map[io.TimeBucketKey][]byte
}
