package twitter

import (
	"context"
	"fmt"

	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type ListBase interface {
	GetMembers(context.Context, *resty.Client) ([]*User, error)
	GetId() int64
	Title() string
}

type List struct {
	Id          uint64
	MemberCount int
	Name        string
	Creator     *User
}

func GetLst(ctx context.Context, client *resty.Client, id uint64) (*List, error) {
	api := listByRestId{}
	api.id = id
	url := makeUrl(&api)

	resp, err := client.R().SetContext(ctx).Get(url)
	if err != nil {
		return nil, err
	}

	list := gjson.GetBytes(resp.Body(), "data.list")
	return parseList(&list)
}

func parseList(list *gjson.Result) (*List, error) {
	if !list.Exists() {
		return nil, fmt.Errorf("the list doesn't exist")
	}
	user_results := list.Get("user_results")
	creator, err := parseUserResults(&user_results)
	if err != nil {
		return nil, err
	}
	id_str := list.Get("id_str")
	member_count := list.Get("member_count")
	name := list.Get("name")

	result := List{}
	result.Creator = creator
	result.Id = id_str.Uint()
	result.MemberCount = int(member_count.Int())
	result.Name = name.String()
	return &result, nil
}

func itemContentsToUsers(itemContents []gjson.Result) []*User {
	users := make([]*User, 0, len(itemContents))
	for _, ic := range itemContents {
		user_results := getResults(ic, timelineUser)
		if user_results.String() == "{}" {
			continue
		}
		u, err := parseUserResults(&user_results)
		if err != nil {
			log.WithFields(log.Fields{
				"user_results": user_results.String(),
				"reason":       err,
			}).Debugf("failed to parse user_results")
			continue
		}
		users = append(users, u)
	}
	return users
}

func getMembers(ctx context.Context, client *resty.Client, api timelineApi, instsPath string) ([]*User, error) {
	api.SetCursor("")
	itemContents, err := getTimelineItemContentsTillEnd(ctx, api, client, instsPath)
	if err != nil {
		return nil, err
	}
	return itemContentsToUsers(itemContents), nil
}

func (list *List) GetMembers(ctx context.Context, client *resty.Client) ([]*User, error) {
	api := listMembers{}
	api.count = 200
	api.id = list.Id
	return getMembers(ctx, client, &api, "data.list.members_timeline.timeline.instructions")
}

func (list *List) GetId() int64 {
	return int64(list.Id)
}

func (list *List) Title() string {
	return fmt.Sprintf("%s(%d)", list.Name, list.Id)
}

type UserFollowing struct {
	creator *User
}

func (fo UserFollowing) GetMembers(ctx context.Context, client *resty.Client) ([]*User, error) {
	api := following{}
	api.count = 200
	api.uid = fo.creator.Id
	return getMembers(ctx, client, &api, "data.user.result.timeline.timeline.instructions")
}

func (fo UserFollowing) GetId() int64 {
	return -int64(fo.creator.Id)
}

func (fo UserFollowing) Title() string {
	name := fmt.Sprintf("%s's Following", fo.creator.ScreenName)
	return name
}
