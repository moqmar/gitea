// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migrations

import (
	"fmt"

	"xorm.io/xorm"
)

func extendNotification(x *xorm.Engine) error {
	type Notification struct {
		Reason uint8 `xorm:"SMALLINT INDEX NOT NULL"`
	}

	if err := x.Sync2(new(Notification)); err != nil {
		return fmt.Errorf("Sync2: %v", err)
	}
	return nil
}
