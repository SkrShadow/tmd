package downloading

import (
	"runtime"
	"sync"

	"github.com/go-resty/resty/v2"
	"github.com/gookit/color"
	"github.com/jmoiron/sqlx"
	"github.com/unkmonster/tmd2/database"
	"github.com/unkmonster/tmd2/internal/utils"
	"github.com/unkmonster/tmd2/twitter"
)

type TweetInEntity struct {
	Tweet  *twitter.Tweet
	Entity *UserEntity
}

func (pt TweetInEntity) GetTweet() *twitter.Tweet {
	return pt.Tweet
}

func (pt TweetInEntity) GetPath() string {
	path, err := pt.Entity.Path()
	if err != nil {
		return ""
	}
	return path
}

// var syncedListUsers = make(map[uint64]map[int64]struct{})
var syncedListUsers sync.Map //lid -> uid

func BatchUserDownload(client *resty.Client, db *sqlx.DB, users []*twitter.User, dir string, listEntityId *int) []*TweetInEntity {
	uidToUser := make(map[uint64]*twitter.User)
	userChan := make(chan *twitter.User, len(users))
	for _, u := range users {
		userChan <- u
		uidToUser[u.Id] = u
	}
	close(userChan)

	getterCount := min(len(users), 2*runtime.GOMAXPROCS(0))
	entityChan := make(chan *UserEntity, len(users))
	tweetChan := make(chan PackgedTweet, MaxDownloadRoutine) // 尽量不让任何下载例程闲置
	errch := make(chan PackgedTweet)
	abortChan := make(chan struct{})
	syncWg := sync.WaitGroup{}
	getterWg := sync.WaitGroup{}
	downloaderWg := sync.WaitGroup{}

	abort := sync.OnceFunc(func() {
		close(abortChan)
	})

	cancelled := func() bool {
		select {
		case <-abortChan:
			return true
		default:
			return false
		}
	}

	panicHandler := func(name string) {
		if p := recover(); p != nil {
			color.Danger.Printf("[%s] panic: %v\n", name, p)
			abort()
		}
	}

	userUpdater := func() {
		defer syncWg.Done()
		defer panicHandler("sync worker")
		for u := range userChan {
			if cancelled() {
				break
			}

			var pathEntity *UserEntity
			var err error

			pe, loaded := syncedUsers.Load(u.Id)
			if !loaded {
				pathEntity, err = syncUserAndEntity(db, u, dir)
				if err != nil {
					color.Error.Tips("[sync worker] failed to sync user or entity %s: %v", u.Title(), err)
					continue
				}
				syncedUsers.Store(u.Id, pathEntity)
				entityChan <- pathEntity

				// 同步所有现存的指向此用户的符号链接
				upath, _ := pathEntity.Path()
				linkds, err := database.GetUserLinks(db, u.Id)
				if err != nil {
					color.Error.Tips("[sync worker] failed to get links to %s: %v", u.Title(), err)
					continue
				}

				for _, linkd := range linkds {
					if err = updateUserLink(linkd, db, upath); err != nil {
						color.Error.Tips("[sync worker] failed to sync link %s: %v", u.Title(), err)
					}
					sl, _ := syncedListUsers.LoadOrStore(*listEntityId, &sync.Map{})
					syncedList := sl.(*sync.Map)
					syncedList.Store(u.Id, struct{}{})
				}

			} else {
				pathEntity = pe.(*UserEntity)
				color.Note.Printf("[sync worker] Skiped synced user '%s'\n", u.Title())
			}

			// 为当前列表的新用户创建符号链接
			if listEntityId == nil {
				continue
			}
			sl, _ := syncedListUsers.LoadOrStore(*listEntityId, &sync.Map{})
			syncedList := sl.(*sync.Map)
			_, loaded = syncedList.LoadOrStore(u.Id, struct{}{})
			if loaded {
				continue
			}

			upath, _ := pathEntity.Path()
			var linkname = pathEntity.Name()
			if runtime.GOOS == "windows" {
				linkname += ".lnk"
			}

			curlink := &database.UserLink{}
			curlink.Name = linkname
			curlink.ParentLstEntityId = int32(*listEntityId)
			curlink.Uid = u.Id

			linkpath, err := curlink.Path(db)
			if err == nil {
				if err = utils.CreateLink(upath, linkpath); err == nil {
					err = database.CreateUserLink(db, curlink)
				}
			}
			if err != nil {
				color.Error.Tips("[sync worker] failed to create link %s: %v", u.Title(), err)
			}

		}
	}

	tweetGetter := func() {
		defer getterWg.Done()
		defer panicHandler("tweet getter")
		for e := range entityChan {
			if cancelled() {
				break
			}
			tweets, err := getTweetAndUpdateLatestReleaseTime(client, uidToUser[e.Uid()], e)
			if utils.IsStatusCode(err, 429) {
				color.Error.Tips("[getting worker]: %v", err)
				abort()
				continue
			}
			if err != nil {
				color.Error.Tips("[getting worker] %s: %v", e.Name(), err)
				continue
			}
			for _, tw := range tweets {
				pt := TweetInEntity{Tweet: tw, Entity: e}
				tweetChan <- &pt
			}
		}
	}

	for i := 0; i < getterCount; i++ {
		syncWg.Add(1)
		go userUpdater()
		getterWg.Add(1)
		go tweetGetter()
	}

	wc := workerController{}
	wc.abort = abort
	wc.cancelled = cancelled
	wc.Wg = &downloaderWg

	for i := 0; i < MaxDownloadRoutine; i++ {
		downloaderWg.Add(1)
		go tweetDownloader(client, wc, errch, tweetChan)
	}

	//closer
	go func() {
		syncWg.Wait()
		close(entityChan)
		//log.Printf("entity chan has closed\n")

		getterWg.Wait()
		close(tweetChan)
		//log.Printf("tweet chan has closed\n")

		downloaderWg.Wait()
		close(errch)
		//log.Printf("failureTw chan has closed\n")
	}()

	failures := []*TweetInEntity{}
	for pt := range errch {
		failures = append(failures, pt.(*TweetInEntity))
	}
	return failures
}

func downloadList(client *resty.Client, db *sqlx.DB, list twitter.ListBase, dir string, realDir string) ([]*TweetInEntity, error) {
	expectedTitle := string(utils.WinFileName([]byte(list.Title())))
	entity := NewListEntity(db, list.GetId(), dir)
	if err := syncPath(entity, expectedTitle); err != nil {
		return nil, err
	}

	members, err := list.GetMembers(client)
	if err != nil || len(members) == 0 {
		return nil, err
	}

	eid := entity.Id()
	return BatchUserDownload(client, db, members, realDir, &eid), nil
}

func syncList(db *sqlx.DB, list *twitter.List) error {
	listdb, err := database.GetLst(db, list.Id)
	if err != nil {
		return err
	}
	if listdb == nil {
		return database.CreateLst(db, &database.Lst{Id: list.Id, Name: list.Name, OwnerId: list.Creator.Id})
	}
	return database.UpdateLst(db, &database.Lst{Id: list.Id, Name: list.Name, OwnerId: list.Creator.Id})
}

func DownloadList(client *resty.Client, db *sqlx.DB, list twitter.ListBase, dir string, realDir string) ([]*TweetInEntity, error) {
	tlist, ok := list.(*twitter.List)
	if ok {
		if err := syncList(db, tlist); err != nil {
			return nil, err
		}
	}
	return downloadList(client, db, list, dir, realDir)
}
