// Copyright (c) 2016 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package store

import (
	"fmt"
	"testing"

	"github.com/mattermost/platform/model"
)

func TestFileInfoSaveGet(t *testing.T) {
	Setup()

	info := &model.FileInfo{
		CreatorId: model.NewId(),
		Path:      "file.txt",
	}

	if result := <-store.FileInfo().Save(info); result.Err != nil {
		t.Fatal(result.Err)
	} else if returned := result.Data.(*model.FileInfo); len(returned.Id) == 0 {
		t.Fatal("should've assigned an id to FileInfo")
	} else {
		info = returned
	}

	if result := <-store.FileInfo().Get(info.Id); result.Err != nil {
		t.Fatal(result.Err)
	} else if returned := result.Data.(*model.FileInfo); returned.Id != info.Id {
		t.Log(info)
		t.Log(returned)
		t.Fatal("should've returned correct FileInfo")
	}

	info2 := Must(store.FileInfo().Save(&model.FileInfo{
		CreatorId: model.NewId(),
		Path:      "file.txt",
		DeleteAt:  123,
	})).(*model.FileInfo)

	if result := <-store.FileInfo().Get(info2.Id); result.Err == nil {
		t.Fatal("shouldn't have gotten deleted file")
	}
}

func TestFileInfoSaveGetByPath(t *testing.T) {
	Setup()

	info := &model.FileInfo{
		CreatorId: model.NewId(),
		Path:      fmt.Sprintf("%v/file.txt", model.NewId()),
	}

	if result := <-store.FileInfo().Save(info); result.Err != nil {
		t.Fatal(result.Err)
	} else if returned := result.Data.(*model.FileInfo); len(returned.Id) == 0 {
		t.Fatal("should've assigned an id to FileInfo")
	} else {
		info = returned
	}

	if result := <-store.FileInfo().GetByPath(info.Path); result.Err != nil {
		t.Fatal(result.Err)
	} else if returned := result.Data.(*model.FileInfo); returned.Id != info.Id {
		t.Log(info)
		t.Log(returned)
		t.Fatal("should've returned correct FileInfo")
	}

	info2 := Must(store.FileInfo().Save(&model.FileInfo{
		CreatorId: model.NewId(),
		Path:      "file.txt",
		DeleteAt:  123,
	})).(*model.FileInfo)

	if result := <-store.FileInfo().GetByPath(info2.Id); result.Err == nil {
		t.Fatal("shouldn't have gotten deleted file")
	}
}

func TestFileInfoGetForPost(t *testing.T) {
	Setup()

	userId := model.NewId()
	postId := model.NewId()

	infos := []*model.FileInfo{
		{
			PostId:    postId,
			CreatorId: userId,
			Path:      "file.txt",
		},
		{
			PostId:    postId,
			CreatorId: userId,
			Path:      "file.txt",
		},
		{
			PostId:    postId,
			CreatorId: userId,
			Path:      "file.txt",
			DeleteAt:  123,
		},
		{
			PostId:    model.NewId(),
			CreatorId: userId,
			Path:      "file.txt",
		},
	}

	for i, info := range infos {
		infos[i] = Must(store.FileInfo().Save(info)).(*model.FileInfo)
	}

	if result := <-store.FileInfo().GetForPost(postId); result.Err != nil {
		t.Fatal(result.Err)
	} else if returned := result.Data.([]*model.FileInfo); len(returned) != 2 {
		t.Fatal("should've returned exactly 2 file infos")
	}
}

func TestFileInfoAttachToPost(t *testing.T) {
	Setup()

	userId := model.NewId()
	postId := model.NewId()

	info1 := Must(store.FileInfo().Save(&model.FileInfo{
		CreatorId: userId,
		Path:      "file.txt",
	})).(*model.FileInfo)

	if len(info1.PostId) != 0 {
		t.Fatal("file shouldn't have a PostId")
	}

	if result := <-store.FileInfo().AttachToPost(info1.Id, postId); result.Err != nil {
		t.Fatal(result.Err)
	} else {
		info1 = Must(store.FileInfo().Get(info1.Id)).(*model.FileInfo)
	}

	if len(info1.PostId) == 0 {
		t.Fatal("file should now have a PostId")
	}

	info2 := Must(store.FileInfo().Save(&model.FileInfo{
		CreatorId: userId,
		Path:      "file.txt",
	})).(*model.FileInfo)

	if result := <-store.FileInfo().AttachToPost(info2.Id, postId); result.Err != nil {
		t.Fatal(result.Err)
	} else {
		info2 = Must(store.FileInfo().Get(info2.Id)).(*model.FileInfo)
	}

	if result := <-store.FileInfo().GetForPost(postId); result.Err != nil {
		t.Fatal(result.Err)
	} else if infos := result.Data.([]*model.FileInfo); len(infos) != 2 {
		t.Fatal("should've returned exactly 2 file infos")
	}
}

func TestFileInfoDeleteForPost(t *testing.T) {
	Setup()

	userId := model.NewId()
	postId := model.NewId()

	infos := []*model.FileInfo{
		{
			PostId:    postId,
			CreatorId: userId,
			Path:      "file.txt",
		},
		{
			PostId:    postId,
			CreatorId: userId,
			Path:      "file.txt",
		},
		{
			PostId:    postId,
			CreatorId: userId,
			Path:      "file.txt",
			DeleteAt:  123,
		},
		{
			PostId:    model.NewId(),
			CreatorId: userId,
			Path:      "file.txt",
		},
	}

	for i, info := range infos {
		infos[i] = Must(store.FileInfo().Save(info)).(*model.FileInfo)
	}

	if result := <-store.FileInfo().DeleteForPost(postId); result.Err != nil {
		t.Fatal(result.Err)
	}

	if infos := Must(store.FileInfo().GetForPost(postId)).([]*model.FileInfo); len(infos) != 0 {
		t.Fatal("shouldn't have returned any file infos")
	}
}
