# TaierSpeedtest

全球网测（泰尔测速）Linux 客户端。协议还原自 `com.cnspeedtest.globalspeed` 4.4.8。

Go 单文件，默认交互：北上广省会 × 电信/联通/移动，单线程 + 多线程对照。本机 IPv6 能上网时自动加测 IPv6。

## 一键运行

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/MiaM1ku/taierspeedtest/main/run.sh)
```

指定测速点：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/MiaM1ku/taierspeedtest/main/run.sh) --points 湖北
```

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

## 发布

推送版本标签后，GitHub Actions 会交叉编译 Linux amd64/arm64 并更新 Release：

```bash
git tag v1.0.0
git push origin v1.0.0
```

## 说明

- 延迟优先 ICMP Ping，否则 HTTP tcping
- 结果图会上传到 111666.best
- 仅供个人网络质量测试
