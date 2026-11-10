package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZaneXo/zsmeet/internal/service"
)

// 这几个命令在服务器上救急用：系统不发邮件，唯一的管理员忘了密码、
// 或者管理员离职没有交接时，网页上没有人能处理，只能在服务器上执行：
//
//	docker compose exec api zsmeet reset-link 邮箱
//	docker compose exec api zsmeet make-admin 邮箱
//
// 有人故意不停地输错某个账号的密码、让它一直被锁着时，用这个马上解开：
//
//	docker compose exec api zsmeet unlock 邮箱

func recoveryService(ctx context.Context) (*service.Service, func(), error) {
	d, err := setup(ctx)
	if err != nil {
		return nil, nil, err
	}
	// 这几个命令只碰账号，用不到文件存储和任务队列
	return service.New(d.pool, d.store, d.settings, nil), d.pool.Close, nil
}

// resetLink 给一个账号生成重置密码的链接并打印出来。
func resetLink(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("用法：zsmeet reset-link 邮箱")
	}
	svc, done, err := recoveryService(ctx)
	if err != nil {
		return err
	}
	defer done()
	token, expires, err := svc.RecoverResetLink(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("重置密码的链接：\n\n    <站点地址>/reset#%s\n\n", token)
	fmt.Println("把 <站点地址> 换成平时打开 zsmeet 的地址，例如 http://192.168.1.10:8080")
	fmt.Printf("链接只能用一次，%s 之前有效。设置新密码后，这个账号在其他设备上的登录会全部失效。\n", expires.Local().Format("1月2日 15:04"))
	return nil
}

// makeAdmin 把一个账号设为管理员。
func makeAdmin(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("用法：zsmeet make-admin 邮箱")
	}
	svc, done, err := recoveryService(ctx)
	if err != nil {
		return err
	}
	defer done()
	user, err := svc.RecoverAdmin(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s（%s）现在是管理员了。\n", user.Name, user.Email)
	return nil
}

// unlock 清掉一个账号的登录失败次数，锁着的话马上解开。
func unlock(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("用法：zsmeet unlock 邮箱")
	}
	svc, done, err := recoveryService(ctx)
	if err != nil {
		return err
	}
	defer done()
	user, ips, err := svc.UnlockLogin(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s（%s）现在可以登录了，输错密码的次数已经清零。\n", user.Name, user.Email)
	if ips > 0 {
		fmt.Printf("另外解开了 %d 个被锁住的 IP。\n", ips)
	}
	fmt.Println("如果是有人在故意输错，过 15 分钟他还能再锁一次；可以在反向代理上挡住对方的 IP。")
	return nil
}
