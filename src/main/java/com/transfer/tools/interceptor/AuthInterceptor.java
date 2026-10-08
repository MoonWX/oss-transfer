package com.transfer.tools.interceptor;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.UserService;
import com.transfer.tools.service.ReportDownloadTicketService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;
import org.springframework.web.servlet.HandlerInterceptor;

import javax.servlet.http.HttpServletRequest;
import javax.servlet.http.HttpServletResponse;
import javax.servlet.DispatcherType;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import java.util.HashMap;
import java.util.Map;

/**
 * 登录认证拦截器：校验除 /api/auth 之外的 API 请求是否携带有效 token。
 */
@Component
public class AuthInterceptor implements HandlerInterceptor {
    private static final Pattern REPORT_DOWNLOAD = Pattern.compile("^/api/master/task-reports/([0-9]+)/download$");
    private static final String REPORT_AUTHENTICATED = AuthInterceptor.class.getName() + ".reportAuthenticated";

    @Autowired
    private UserService userService;

    @Autowired
    private ObjectMapper objectMapper;

    @Autowired
    private DistributedConfig distributedConfig;

    @Autowired
    private ReportDownloadTicketService reportDownloadTicketService;

    @Override
    public boolean preHandle(HttpServletRequest request, HttpServletResponse response, Object handler) throws Exception {
        // 放行登录相关接口
        String uri = request.getRequestURI();
        // These endpoints perform their own one-use invitation or per-Agent authentication.
        if (uri.equals("/api/agents/join") || uri.equals("/api/agents/poll")
                || uri.equals("/api/agents/result") || uri.equals("/api/agents/relay")
                || uri.startsWith("/api/agents/bootstrap/") || uri.startsWith("/api/agents/package/")) return true;
        // This endpoint returns only a nonce-bound proof, never the shared credential.
        if ("GET".equals(request.getMethod()) && "/api/node-registration/proof".equals(uri)) return true;
        Matcher download = REPORT_DOWNLOAD.matcher(uri);
        boolean reportDownload = "POST".equals(request.getMethod()) && download.matches();
        if (reportDownload && request.getDispatcherType() == DispatcherType.ASYNC
                && Boolean.TRUE.equals(request.getAttribute(REPORT_AUTHENTICATED))) return true;
        if (uri.startsWith("/api/auth/")) {
            return true;
        }

        // 放行静态资源
        if (!uri.startsWith("/api/")) {
            return true;
        }

        // 放行节点间内部调用：携带正确的 X-Internal-Token 时视为可信主节点请求
        String internalToken = request.getHeader("X-Internal-Token");
        String expectedToken = distributedConfig.getInternalToken();
        if (internalToken != null && !internalToken.isEmpty()
                && expectedToken != null && !expectedToken.isEmpty()
                && internalToken.equals(expectedToken)) {
            return true;
        }

        String authorization = request.getHeader("Authorization");
        String token = extractToken(authorization);
        String username = userService.getUsernameByToken(token);

        // Only this download endpoint accepts a one-use ticket, in the POST body.
        // General API tokens and tickets are never accepted from URL parameters.
        if (username == null && reportDownload && request.getQueryString() == null
                && request.getContentType() != null
                && request.getContentType().split(";", 2)[0].trim().equalsIgnoreCase("application/x-www-form-urlencoded")) {
            try {
                username = reportDownloadTicketService.consume(request.getParameter("ticket"), Integer.valueOf(download.group(1)));
            } catch (NumberFormatException ignored) { /* Invalid task ID remains unauthorized. */ }
        }

        if (username == null) {
            response.setContentType("application/json;charset=UTF-8");
            response.setStatus(HttpServletResponse.SC_UNAUTHORIZED);
            Map<String, Object> result = new HashMap<>();
            result.put("success", false);
            result.put("message", reportDownload ? "下载授权已失效，请重新导出报告" : "未登录或会话已过期，请重新登录");
            response.getWriter().write(objectMapper.writeValueAsString(result));
            return false;
        }

        request.setAttribute("currentUser", username);
        if (reportDownload) request.setAttribute(REPORT_AUTHENTICATED, Boolean.TRUE);
        return true;
    }

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
