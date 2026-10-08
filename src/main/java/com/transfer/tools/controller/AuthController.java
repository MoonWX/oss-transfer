package com.transfer.tools.controller;

import com.transfer.tools.model.User;
import com.transfer.tools.service.UserService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.web.bind.annotation.*;

import java.util.HashMap;
import java.util.Map;

/**
 * 登录认证接口。
 */
@Slf4j
@RestController
@RequestMapping("/api/auth")
public class AuthController {

    @Autowired
    private UserService userService;

    /**
     * 登录接口。
     * 请求体：{ "username": "...", "password": "..." }
     * 成功返回 token 与用户信息。
     */
    @PostMapping("/login")
    public Map<String, Object> login(@RequestBody Map<String, String> body) {
        Map<String, Object> result = new HashMap<>();
        String username = body.get("username");
        String password = body.get("password");
        if (username == null || username.trim().isEmpty() || password == null || password.isEmpty()) {
            result.put("success", false);
            result.put("message", "用户名和密码不能为空");
            return result;
        }
        String token = userService.login(username.trim(), password);
        if (token == null) {
            result.put("success", false);
            result.put("message", "用户名或密码错误");
            return result;
        }
        User user = userService.getUserInfo(username.trim());
        result.put("success", true);
        result.put("token", token);
        result.put("user", user);
        return result;
    }

    /**
     * 登出接口。
     */
    @PostMapping("/logout")
    public Map<String, Object> logout(@RequestHeader(value = "Authorization", required = false) String authorization) {
        Map<String, Object> result = new HashMap<>();
        String token = extractToken(authorization);
        userService.logout(token);
        result.put("success", true);
        result.put("message", "已退出登录");
        return result;
    }

    /**
     * 获取当前登录用户信息。
     */
    @GetMapping("/current")
    public Map<String, Object> current(@RequestHeader(value = "Authorization", required = false) String authorization) {
        Map<String, Object> result = new HashMap<>();
        String token = extractToken(authorization);
        String username = userService.getUsernameByToken(token);
        if (username == null) {
            result.put("success", false);
            result.put("message", "未登录或会话已过期");
            return result;
        }
        User user = userService.getUserInfo(username);
        result.put("success", true);
        result.put("user", user);
        return result;
    }

    /**
     * 修改密码。
     * 请求体：{ "oldPassword": "...", "newPassword": "..." }
     */
    @PostMapping("/change-password")
    public Map<String, Object> changePassword(@RequestHeader(value = "Authorization", required = false) String authorization,
                                              @RequestBody Map<String, String> body) {
        Map<String, Object> result = new HashMap<>();
        String token = extractToken(authorization);
        String username = userService.getUsernameByToken(token);
        if (username == null) {
            result.put("success", false);
            result.put("message", "未登录或会话已过期");
            return result;
        }
        String oldPassword = body.get("oldPassword");
        String newPassword = body.get("newPassword");
        if (oldPassword == null || newPassword == null || newPassword.isEmpty()) {
            result.put("success", false);
            result.put("message", "原密码和新密码不能为空");
            return result;
        }
        boolean changed = userService.changePassword(username, oldPassword, newPassword);
        if (!changed) {
            result.put("success", false);
            result.put("message", "原密码错误");
            return result;
        }
        result.put("success", true);
        result.put("message", "密码修改成功");
        return result;
    }

    /**
     * 从 Authorization 头提取 token，支持 "Bearer xxx" 或直接 token。
     */
    private String extractToken(String authorization) {
        if (authorization == null) {
            return null;
        }
        if (authorization.startsWith("Bearer ")) {
            return authorization.substring(7).trim();
        }
        return authorization.trim();
    }
}