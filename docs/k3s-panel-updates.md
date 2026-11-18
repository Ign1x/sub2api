# k3s 中保留面板升级和 Mihomo 数据

面板升级下载 owner fork 的正式发布包，校验摘要后替换当前可执行文件，再等待服务重启。若运行 `/app/sub2api` 且没有持久化卷，kubelet 重新启动容器会恢复镜像里的二进制；容器内的 Mihomo 配置也会丢失。

需要同时持久化应用数据和实际运行的二进制：

- 将独立的应用数据 PVC 挂到 `/app/data`，设置 `DATA_DIR=/app/data`。
- 将已校验的 Sub2API 二进制放在 `/app/data/bin/sub2api`，使用该路径作为容器 command。
- 使用 initContainer 仅在目标二进制不存在时从 runtime 镜像复制初始版本。已有二进制不得被 initContainer 或镜像更新覆盖。
- 数据库、Redis 和加密密钥继续使用原有配置。迁移前备份数据库、Deployment、当前二进制和完整 `/app/data`，将现有数据复制到新卷，再切换启动路径。

Deployment 的核心配置示例（PVC、namespace 和镜像按实际部署填写）：

```yaml
spec:
  template:
    spec:
      volumes:
        - name: application-data
          persistentVolumeClaim:
            claimName: sub2api-data
      initContainers:
        - name: seed-panel-binary
          image: ghcr.io/ign1x/sub2api:latest
          command:
            - sh
            - -ec
            - |
              mkdir -p /persistent/bin
              if [ ! -x /persistent/bin/sub2api ]; then
                cp /app/sub2api /persistent/bin/sub2api
                chmod 755 /persistent/bin/sub2api
              fi
          volumeMounts:
            - name: application-data
              mountPath: /persistent
      containers:
        - name: sub2api
          command: [/app/data/bin/sub2api]
          env:
            - name: DATA_DIR
              value: /app/data
          volumeMounts:
            - name: application-data
              mountPath: /app/data
```

后续在面板点击“立即更新”和“重启服务”。k3s 重启后仍执行卷里的新二进制，`.backup` 留在同一个卷里供回退。运行镜像用于提供基础系统和首次安装，不决定已经安装的面板版本。容器退出后的自动恢复由 kubelet 管理。

验收时核对 `/proc/1/exe` 指向持久化目录、新二进制 `--version`、站点返回版本、Mihomo 配置摘要与节点数量。完成一次容器重启后再次核对版本和配置，才能证明面板升级可以保留。数据库迁移不会因二进制回退自动撤销。
