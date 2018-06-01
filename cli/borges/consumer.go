package main

import (
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/src-d/borges"
	"github.com/src-d/borges/lock"
	"github.com/src-d/borges/storage"

	"gopkg.in/src-d/core-retrieval.v0/repository"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/osfs"
	"gopkg.in/src-d/go-log.v1"
	"gopkg.in/src-d/go-queue.v1"
	_ "gopkg.in/src-d/go-queue.v1/amqp"
)

const (
	consumerCmdName      = "consumer"
	consumerCmdShortDesc = "consume jobs from a queue and process them"
	consumerCmdLongDesc  = "This consumer fetches, packs and stores repositories. It reads one job per repository. Jobs should be produced witht he producer command."
)

var consumerCommand = &consumerCmd{consumerSubcmd: newConsumerSubcmd(
	consumerCmdName,
	consumerCmdShortDesc,
	consumerCmdLongDesc,
)}

type consumerSubcmd struct {
	command

	Locking string `long:"locking" env:"BORGES_LOCKING" default:"local:" description:"locking service configuration"`
	Workers int    `long:"workers" env:"BORGES_WORKERS" default:"1" description:"number of workers"`
	Timeout string `long:"timeout" env:"BORGES_TIMEOUT" default:"10h" description:"deadline to process a job"`

	RootRepositoriesDir     string `long:"root-repositories-dir" env:"BORGES_ROOT_REPOSITORIES_DIR" default:"/tmp/root-repositories" description:"path to the directory storing rooted repositories (can be local path or hdfs://)"`
	RootRepositoriesTempDir string `long:"root-repositories-temp-dir" env:"BORGES_ROOT_REPOSITORIES_DIR" default:"/tmp/root-repositories-dot-copy"`
	BucketSize              int    `long:"bucket-size" env:"BORGES_BUCKET_SIZE" default:"0" description:"if higher than zero, repositories are stored in bucket directories with a prefix of the given amount of characters from its root hash"`

	TempDir      string `long:"temp-dir" env:"BORGES_TEMP_DIR" default:"/tmp/borges" description:"path of temporary directory to clone repositories into"`
	CleanTempDir bool   `long:"temp-dir-clean" env:"BORGES_TEMP_DIR_CLEAN" description:"clean temporary directory before starting"`
}

func newConsumerSubcmd(name, short, long string) consumerSubcmd {
	return consumerSubcmd{command: newCommand(
		name,
		short,
		long,
	)}
}

type consumerCmd struct {
	consumerSubcmd
	databaseOpts
}

func (c *consumerCmd) Execute(args []string) error {
	c.init()

	db, err := c.openDatabase()
	if err != nil {
		return err
	}

	tmp, err := c.newTemporaryFilesystem()
	if err != nil {
		return err
	}

	txer, err := c.newRootedTransactioner(tmp)
	if err != nil {
		return err
	}

	locking, err := lock.New(c.Locking)
	if err != nil {
		return err
	}

	b, err := queue.NewBroker(c.Broker)
	if err != nil {
		return err
	}

	defer b.Close()
	q, err := b.Queue(c.Queue)
	if err != nil {
		return err
	}

	timeout, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return err
	}

	wp := borges.NewArchiverWorkerPool(
		storage.FromDatabase(db),
		txer,
		borges.NewTemporaryCloner(tmp),
		locking,
		timeout,
	)
	wp.SetWorkerCount(c.Workers)

	ac := borges.NewConsumer(q, wp)

	var term = make(chan os.Signal)
	var done = make(chan struct{})
	go func() {
		select {
		case <-term:
			log.Infof("signal received, stopping...")
			ac.Stop()
		case <-done:
			ac.Stop()
		}
	}()
	signal.Notify(term, syscall.SIGTERM, os.Interrupt)

	err = ac.Start()
	close(done)

	return err
}

func (c *consumerSubcmd) newTemporaryFilesystem() (billy.Filesystem, error) {
	if c.CleanTempDir {
		os.RemoveAll(c.TempDir)
	}

	if err := os.MkdirAll(c.TempDir, os.FileMode(0755)); err != nil {
		return nil, err
	}

	dir, err := ioutil.TempDir(c.TempDir, "")
	if err != nil {
		return nil, err
	}

	return osfs.New(dir), nil
}

func (c *consumerSubcmd) newRootedTransactioner(tmp billy.Filesystem) (
	repository.RootedTransactioner, error) {
	tmp, err := tmp.Chroot("transactioner")
	if err != nil {
		return nil, err
	}

	var remote repository.Fs
	if strings.HasPrefix(c.RootRepositoriesDir, "hdfs://") {
		u, err := url.Parse(c.RootRepositoriesDir)
		if err != nil {
			return nil, err
		}

		path := u.Path
		u.Path = ""

		remote = repository.NewHDFSFs(
			u.String(),
			path,
			c.RootRepositoriesTempDir,
		)
	} else {
		remote = repository.NewLocalFs(osfs.New(c.RootRepositoriesDir))
	}

	txer := repository.NewSivaRootedTransactioner(
		repository.NewCopier(
			tmp,
			remote,
			c.BucketSize,
		),
	)

	return txer, nil
}

func init() {
	_, err := parser.AddCommand(
		consumerCommand.Name(),
		consumerCommand.ShortDescription(),
		consumerCommand.LongDescription(),
		consumerCommand)

	if err != nil {
		panic(err)
	}
}
