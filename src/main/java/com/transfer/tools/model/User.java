package com.transfer.tools.model;

import lombok.Data;

/**
 * 系统用户模型，用于登录认证。
 * 用户信息存储在 RocksDB 中，密码以加盐哈希形式保存。
 */
@Data
public class User {

    /** 用户名，唯一标识 */
    private String username;

    /** 密码哈希（格式：salt:hash），不保存明文密码 */
    private String passwordHash;

    /** 显示名称 */
    private String displayName;

    /** 角色：admin / user */
    private String role;

    /** 创建时间（毫秒时间戳） */
    private long createTime;

    /** 最近登录时间（毫秒时间戳） */
    private long lastLoginTime;
}