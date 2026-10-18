# 用本地源码替换线上 8765 应用

2026-10-18 已只读核实：服务器 `108.186.246.69` 为 x86_64，Compose 项目名 `sub2api8765`，目录 `/opt/sub2api-8765`，配置由 `docker-compose.yml` 与 `docker-compose.override.yml` 组成。服务名 `sub2api`，容器名 `sub2api-8765`，端口 `8765:8080`，数据目录 `/opt/sub2api-8765/data:/app/data`。运行程序实际为 0.2.17。

最新状态：用户随后明确授权“开始替换”，已于 2026-10-18 08:37（Asia/Shanghai）完成部署，当前运行 `0.2.17-local-streamfix.20261018`，验收结果见文末。

以下为完整操作步骤；本次已执行范围见文末记录。按顺序操作，任何一步报错都先停止处理。镜像构建包含当前工作目录里的未提交修改，无需先推送 GitHub。切换会短暂中断服务，选择低流量时段并先让长连接请求结束；无法保证正在进行的流式请求不中断。

## 1. 在本机 Mac 构建并上传镜像

先启动 Docker Desktop，确保 `docker info` 成功。使用仓库根目录的 Dockerfile，它会构建前端并嵌入后端；不要只运行普通 `make build` 当作完整发布包。显式指定服务器架构，避免生成 Mac ARM 镜像。

```bash
cd /Users/lvv/code/sub2api
docker info
docker buildx build --platform linux/amd64 --load \
  --build-arg VERSION=0.2.17-local-streamfix.20261018 \
  --build-arg COMMIT="$(git rev-parse --short HEAD)-local" \
  -t sub2api-local:streamfix-20261018 .

docker image inspect sub2api-local:streamfix-20261018 \
  --format '{{.Os}}/{{.Architecture}}'
docker save -o /tmp/sub2api-streamfix-20261018.tar sub2api-local:streamfix-20261018
scp /tmp/sub2api-streamfix-20261018.tar root@108.186.246.69:/opt/sub2api-8765/
```

架构应输出 `linux/amd64`。这是独立的自定义版本号，方便与官方 0.2.17 区分。以后重新构建请换一个新标签和版本后缀，避免覆盖本次发布记录。

## 2. 登录服务器，备份并载入镜像

以下在服务器同一个 Bash 会话中执行。数据库备份使用 PostgreSQL 容器自己的账号配置，不需要复制密码。

```bash
ssh root@108.186.246.69
bash
set -euo pipefail
cd /opt/sub2api-8765
umask 077
backup_dir="/opt/sub2api-8765/backups/pre-streamfix-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$backup_dir"
cp -a docker-compose.yml docker-compose.override.yml "$backup_dir/"
if [ -f .env ]; then cp -a .env "$backup_dir/"; fi
tar --exclude=data/logs -czf "$backup_dir/app-data.tgz" data

docker exec sub2api-8765-postgres sh -ec \
  'pg_dump -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" -Fc' \
  > "$backup_dir/database.dump"
test -s "$backup_dir/database.dump"
docker exec -i sub2api-8765-postgres pg_restore --list \
  < "$backup_dir/database.dump" > "$backup_dir/database.contents.txt"

rollback_image="sub2api-local:before-streamfix-$(date +%Y%m%d-%H%M%S)"
docker commit sub2api-8765 "$rollback_image"
printf '%s\n' "$rollback_image" > "$backup_dir/rollback-image.txt"
docker save -o "$backup_dir/rollback-image.tar" "$rollback_image"
docker load -i /opt/sub2api-8765/sub2api-streamfix-20261018.tar
docker run --rm --entrypoint /app/sub2api \
  sub2api-local:streamfix-20261018 -version
```

这里特意备份当前容器的文件系统：线上程序曾在容器内升级，原来的 `weishaw/sub2api:latest` 镜像不代表当前运行的 0.2.17，不能直接拿它回滚。`docker commit` 会短暂停顿容器，且不包含挂载的数据，因此另做了数据库和 data 目录备份。回滚镜像可能包含容器配置中的敏感信息，仅保存在受限服务器目录，不推送公开仓库。

## 3. 增加本地镜像配置并替换应用

新增一个 Compose 覆盖文件，继承线上已有的端口、网络、数据库、Redis、密钥和数据挂载。下面的存在检查用于避免覆盖已保存的发布配置；若提示文件已存在，先检查其内容再决定下一步。

```bash
test ! -e docker-compose.local-build.yml
cat > docker-compose.local-build.yml <<'YAML'
services:
  sub2api:
    image: sub2api-local:streamfix-20261018
    pull_policy: never
    environment:
      GATEWAY_STREAM_OUTPUT_TOKEN_ESTIMATION: "true"
YAML

export COMPOSE_PROJECT_NAME=sub2api8765
export COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:docker-compose.local-build.yml
docker compose config --quiet
docker compose up -d --no-deps --force-recreate --pull never --timeout 120 sub2api
```

只重建 `sub2api`，不停止数据库、Redis，不执行 `docker compose down -v`。应用启动时自动执行尚未应用的数据库迁移，包括新增 `usage_logs.output_tokens_estimated` 的迁移 246；无需手动执行 SQL。启动迁移可能需要等待锁，应用健康后才算部署完成。

## 4. 检查版本、健康状态和迁移

```bash
docker exec sub2api-8765 /app/sub2api -version
docker inspect sub2api-8765 --format '{{.Config.Image}} {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}'
docker logs --tail 100 sub2api-8765
curl -fsS http://127.0.0.1:8765/health
docker exec sub2api-8765-postgres sh -ec \
  'psql -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" -c "SELECT filename FROM schema_migrations WHERE filename = '\''246_add_output_tokens_estimated.sql'\'';"'
```

版本应为 `0.2.17-local-streamfix.20261018`，镜像应为 `sub2api-local:streamfix-20261018`，健康状态最终为 `healthy`，迁移查询应返回一条记录。若尚在启动，检查日志并稍后再验，不要只凭 Compose 命令退出成功判断发布成功。

浏览器刷新 `http://108.186.246.69:8765`，确认登录、原有用户/余额/账号仍正常。发起一条受控的正常请求，检查生成及账单；估算只在缺少完整上游计量且有可估算输出时出现，不会给所有请求显示“估算”。历史账单不会自动补扣。模拟断流的计费验收应在测试账号和可控上游上完成，不通过停止生产容器制造故障。

## 5. 需要回滚时

若仍在上面的同一会话，可直接使用 `$rollback_image`；若已重新登录，先从对应备份目录的 `rollback-image.txt` 读取确切镜像名。保留数据库新列和已经产生的账单。

```bash
cd /opt/sub2api-8765
# 重新登录时先设置 rollback_image 为备份中的确切镜像名。
test -n "${rollback_image:-}"
docker image inspect "$rollback_image" >/dev/null
cat > docker-compose.local-build.yml <<YAML
services:
  sub2api:
    image: ${rollback_image}
    pull_policy: never
    environment:
      GATEWAY_STREAM_OUTPUT_TOKEN_ESTIMATION: "false"
YAML
export COMPOSE_PROJECT_NAME=sub2api8765
export COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:docker-compose.local-build.yml
docker compose config --quiet
docker compose up -d --no-deps --force-recreate --pull never --timeout 120 sub2api
docker exec sub2api-8765 /app/sub2api -version
curl -fsS http://127.0.0.1:8765/health
```

普通程序回滚不恢复旧数据库快照，否则会丢失备份之后的账单、余额和配置变更。若只是关闭估算，将覆盖文件内开关设为 `"false"`，保持新镜像，再重建应用即可。

## 后续维护

每次新开 SSH 会话运行 Compose 前，都要重新设置上面的 `COMPOSE_PROJECT_NAME` 与包含三个文件的 `COMPOSE_FILE`，或者在命令中显式写出 `-p sub2api8765` 和三个 `-f` 参数。只使用原来的两个文件会重新切回官方镜像。

当前修复只存在于本地源码；后台的官方在线更新可能用官方程序覆盖它。后续更新应先将上游改动合入本地代码，再构建新自定义镜像，重复备份、替换和验证流程。

## 本次准备执行记录（2026-10-18 08:36，Asia/Shanghai）

用户明确要求：完成替换前准备，等用户另行通知开始替换。已完成第 1、2 步，以及第 3 步的覆盖文件生成和配置检查；停在 `docker compose up` 之前。未重建、重启应用，未执行数据库迁移或历史补扣。

- 新镜像：`sub2api-local:streamfix-20261018`，架构 `linux/amd64`。
- 新程序独立无网络运行 `-version` 成功：`0.2.17-local-streamfix.20261018`，commit `b0a02a330-local`，构建时间 `2026-10-18T00:34:04Z`。该检查直接退出，不连接数据库。
- 镜像 ID：`sha256:f5f5dc309e30b766337e4ba302eff63706130e4a4744896f5df2b61860087803`。
- 本地镜像包：`/tmp/sub2api-streamfix-20261018.tar`；服务器镜像包：`/opt/sub2api-8765/sub2api-streamfix-20261018.tar`，已载入 Docker。
- 镜像包 SHA-256：`ab396512e9a16015d67bc17bdd17c914ade73c589a8aa0fd750b5c28086c8898`，两端一致。
- 服务器备份目录：`/opt/sub2api-8765/backups/pre-streamfix-20261018-083328`。数据库备份约 71 MiB，`pg_restore --list` 校验通过；另含原 Compose、`.env`、排除日志的 data 目录备份、校验和及原容器/程序身份记录。未做完整恢复演练。
- 回滚镜像：`sub2api-local:before-streamfix-20261018-083343`，已独立验证其程序版本为原来的 0.2.17；归档保存在备份目录的 `rollback-image.tar`，约 138 MiB。
- 已准备 `/opt/sub2api-8765/docker-compose.local-build.yml`。合并配置检查通过；与原配置逐项比较，只有应用镜像、拉取策略和估算开关变化。
- 快照暂停期间健康状态曾暂时显示异常，随后恢复 `healthy`，最终 `/health` 返回 `{"status":"ok"}`。原容器 ID、启动时间、镜像 ID 和运行程序 SHA-256 与准备前一致。

下次获得用户明确“开始替换”指令后，先复查线上状态和准备文件；若间隔较长，重新备份当前数据库。不要重跑覆盖文件的创建步骤。后续替换命令为：

```bash
cd /opt/sub2api-8765
docker compose -p sub2api8765 \
  -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.local-build.yml \
  up -d --no-deps --force-recreate --pull never --timeout 120 sub2api
```

准备阶段未执行此命令；后续取得明确授权后的执行情况如下。

## 正式替换执行记录（2026-10-18 08:37–08:39，Asia/Shanghai）

用户明确发出“开始替换”后完成以下操作：

- 复查原容器健康状态、待部署镜像 ID、Compose 覆盖文件和回滚备份，并重新备份替换前数据库。新备份为原备份目录下的 `database-before-replace.dump`，归档目录校验通过，同时保存 SHA-256。
- 执行上述 `docker compose ... up -d --no-deps --force-recreate --pull never --timeout 120 sub2api`。新容器 ID 为 `c17eea070cb79554e1f0d85880e5f2977e38e7d4e88e05864ea27a8017e7d663`，版本 `0.2.17-local-streamfix.20261018`。
- 比对替换前后容器列表，只有应用容器改变，数据库和 Redis 容器 ID 均保持原值；端口仍为 `8765:8080`。
- 数据库自动迁移成功：`schema_migrations` 中存在 `246_add_output_tokens_estimated.sql`，新列为 `boolean NOT NULL DEFAULT false`。运行环境中的 `GATEWAY_STREAM_OUTPUT_TOKEN_ESTIMATION=true` 已确认。
- 新容器健康状态为 `healthy`，重启计数为 0；服务器本机及外部 `/health` 均正常。外部首页、嵌入前端脚本和 `/api/v1/settings/public` 均返回 HTTP 200，公开设置返回成功业务码。
- 验收采样时，部署后已有 83 条真实流量账单写入，其中估算标记为 true 的数量为 0；检查范围内未发现 `gateway.record_usage_failed`。没有人为制造生产断流，也未以付费请求模拟估算场景，因此不能把这次部署检查表述为线上断流估算端到端验收。
- 日志中有一条上游 `400 Invalid signature in thinking block` 请求错误，产生 3 条错误日志。历史日志中该错误在替换前已存在（最近一条为 08:36:41），并非本次上线后首次出现；没有对该独立问题做代码或上游配置修改。
- 保存了 `replacement-started.txt`、`replacement-verified.txt`、前后容器列表和 `new-container-startup.log`，均位于服务器原备份目录。历史账单未补扣。
