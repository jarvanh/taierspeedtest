# TaierSpeedtest

全球网测（泰尔测速）Linux 客户端。协议还原自 `com.cnspeedtest.globalspeed` 4.4.8。

单个静态 Go 二进制，不依赖系统字体或 ImageMagick。默认交互：北上广省会 × 电信/联通/移动，单线程 + 多线程对照。本机 IPv6 能访问互联网时自动加测 IPv6。

## 一键运行

任意 Linux amd64/arm64 机器，无需预装依赖：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/MiaM1ku/taierspeedtest/main/run.sh)
```

指定测速点：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/MiaM1ku/taierspeedtest/main/run.sh) --points 湖北
```

`run.sh` 只下载对应架构的 Release 并执行。二进制内嵌中文字体，可直接生成 PNG。

## 本地编译

```bash
git clone https://github.com/MiaM1ku/taierspeedtest.git
cd taierspeedtest
go build -o taierspeedtest .
./taierspeedtest
```

## 交互

直接运行后按提示输入：

- **测速点**：回车 = 北上广省会三网；也可 `湖北`、`北京,上海,浙江`、`武汉电信,杭州联通`
- **模式**：回车 = 单线程 + 多线程对照

## 结果图

测速结束后生成 PNG，并按以下顺序上传：

1. [111666.best](https://111666.best/)（与 NodeQuality 相同图床，仅接受位图）
2. catbox
3. 0x0.st

全部失败时打印本地 PNG 路径。图中只显示城市和运营商，不包含 IP。

## 发布

推送版本标签后，GitHub Actions 会交叉编译 Linux amd64/arm64 并更新 Release：

```bash
git tag v1.0.3
git push origin v1.0.3
```

## 说明

- 延迟优先 ICMP Ping，否则 HTTP tcping
- 仅供个人网络质量测试
