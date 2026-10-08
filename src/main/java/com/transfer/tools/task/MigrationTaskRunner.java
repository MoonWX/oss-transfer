package com.transfer.tools.task;

import com.transfer.tools.service.BucketExploreService;
import com.transfer.tools.service.SubTaskManagerService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.ApplicationArguments;
import org.springframework.boot.ApplicationRunner;
import org.springframework.stereotype.Component;

@Slf4j
@Component
public class MigrationTaskRunner implements ApplicationRunner {

    @Autowired
    private BucketExploreService bucketExploreService;

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Override
    public void run(ApplicationArguments args) throws Exception {
        log.info("========== 对象存储迁移工具启动 ==========");
        log.info("请通过以下API接口控制迁移任务：");
        log.info("  POST /api/migration/explore  - 探索bucket并创建子任务");
        log.info("  GET  /api/migration/status   - 查询迁移状态");
        log.info("  POST /api/migration/start    - 启动迁移任务");
        log.info("  POST /api/migration/stop     - 停止迁移任务");
        log.info("  POST /api/migration/clear    - 清空所有任务");
        log.info("======================================");
    }
}
