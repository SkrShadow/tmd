package downloading

import (
	"net/http"
	"os"

	"github.com/jmoiron/sqlx"
	"github.com/unkmonster/tmd2/database"
	"github.com/unkmonster/tmd2/internal/utils"
	"github.com/unkmonster/tmd2/twitter"
)

func syncUser(db *sqlx.DB, user *twitter.User) error {
	isNew := false
	usrdb, err := database.GetUserById(db, user.Id)
	if err != nil {
		return err
	}

	if usrdb == nil {
		isNew = true
		usrdb = &database.User{}
		usrdb.Id = user.Id
	}

	usrdb.FriendsCount = user.FriendsCount
	usrdb.IsProtected = user.IsProtected
	usrdb.Name = user.Name
	usrdb.ScreenName = user.ScreenName

	if isNew {
		return database.CreateUser(db, usrdb)
	}
	return database.UpdateUser(db, usrdb)
}

func downloadUser(db *sqlx.DB, client *http.Client, user *twitter.User, entity *UserEntity) ([]*twitter.Tweet, error) {
	if err := syncUser(db, user); err != nil {
		return nil, err
	}

	expectedTitle := string(utils.WinFileName([]byte(user.Title())))
	if entity.Title() != expectedTitle {
		if err := entity.Rename(expectedTitle); err != nil {
			return nil, err
		}
	}

	tweets, err := user.GetMeidas(client, &utils.TimeRange{Min: entity.LatestReleaseTime()})
	if err != nil || len(tweets) == 0 {
		return nil, err
	}
	entity.SetLatestReleaseTime(tweets[0].CreatedAt)

	path, err := entity.Path()
	if err != nil {
		return nil, err
	}
	os.Mkdir(path, 0755) // ensure dir of user is exists

	failures := BatchDownloadTweet(client, path, tweets)
	return failures, nil
}

func DownloadUser(db *sqlx.DB, client *http.Client, user *twitter.User, dir string) ([]*twitter.Tweet, error) {
	user, entity, err := syncUserAndEntityInDir(db, user, dir)
	if err != nil {
		return nil, err
	}
	tweets, err := getTweetAndUpdateLatestReleaseTime(client, user, entity)
	if err != nil || len(tweets) == 0 {
		return nil, err
	}

	path, err := entity.Path()
	if err != nil {
		return nil, err
	}
	failures := BatchDownloadTweet(client, path, tweets)
	return failures, nil
}

func syncUserAndEntityInDir(db *sqlx.DB, user *twitter.User, dir string) (*twitter.User, *UserEntity, error) {
	if err := syncUser(db, user); err != nil {
		return nil, nil, err
	}
	expectedTitle := string(utils.WinFileName([]byte(user.Title())))

	newUser := false
	userdb, err := database.LocateUserEntityInDir(db, user.Id, dir)
	if err != nil {
		return nil, nil, err
	}
	if userdb == nil {
		userdb = &database.UserEntity{}
		userdb.ParentDir.Scan(dir)
		userdb.Title = expectedTitle
		userdb.Uid = user.Id
		newUser = true
	}

	entity := UserEntity{dbentity: userdb, db: db}
	if !newUser {
		// 重命名检测
		if entity.Title() != expectedTitle {
			if err := entity.Rename(expectedTitle); err != nil {
				return nil, nil, err
			}
		} else {
			path, err := entity.Path()
			if err != nil {
				return nil, nil, err
			}
			os.Mkdir(path, 0755)
		}

		return user, &entity, nil
	}

	if err := entity.Create(); err != nil {
		return nil, nil, err
	}
	return user, &entity, nil
}

func getTweetAndUpdateLatestReleaseTime(client *http.Client, user *twitter.User, entity *UserEntity) ([]*twitter.Tweet, error) {
	tweets, err := user.GetMeidas(client, &utils.TimeRange{Min: entity.LatestReleaseTime()})
	if err != nil || len(tweets) == 0 {
		return nil, err
	}
	if err := entity.SetLatestReleaseTime(tweets[0].CreatedAt); err != nil {
		return nil, err
	}
	return tweets, nil
}
