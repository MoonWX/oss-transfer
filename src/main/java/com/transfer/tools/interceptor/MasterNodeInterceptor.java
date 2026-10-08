package com.transfer.tools.interceptor;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;
import org.springframework.web.servlet.HandlerInterceptor;

import javax.servlet.http.HttpServletRequest;
import javax.servlet.http.HttpServletResponse;
import java.util.HashMap;
import java.util.Map;

@Component
public class MasterNodeInterceptor implements HandlerInterceptor {

    @Autowired
    private DistributedConfig distributedConfig;

    @Autowired
    private ObjectMapper objectMapper;

    @Override
    public boolean preHandle(HttpServletRequest request, HttpServletResponse response, Object handler) throws Exception {
        if (!distributedConfig.isMaster()) {
            String uri = request.getRequestURI();
            boolean masterApi = uri.startsWith("/api/master/");
            if (!masterApi) {
                // Redirect document entry points only. APIs, health checks and assets remain local.
                if (!("GET".equals(request.getMethod()) || "HEAD".equals(request.getMethod()))
                        || request.getDispatcherType() != javax.servlet.DispatcherType.REQUEST
                        || uri.startsWith("/api/") || uri.startsWith("/actuator/")
                        || uri.startsWith("/assets/") || "/error".equals(uri)
                        || "/health".equals(uri) || "/favicon.ico".equals(uri)
                        || (uri.substring(uri.lastIndexOf('/') + 1).contains(".")
                            && !"/index.html".equals(uri) && !"/login.html".equals(uri))) return true;
                try {
                    String origin = distributedConfig.getMasterWebUrl();
                    String query = request.getQueryString();
                    // origin is server configuration; request Host/forwarding/returnUrl are never used.
                    String path = uri.startsWith("/") ? uri : "/" + uri;
                    if (path.indexOf('\r') >= 0 || path.indexOf('\n') >= 0
                            || (query != null && (query.indexOf('\r') >= 0 || query.indexOf('\n') >= 0)))
                        throw new IllegalArgumentException();
                    response.setStatus(HttpServletResponse.SC_FOUND);
                    response.setHeader("Location", origin + path + (query == null ? "" : "?" + query));
                    response.setHeader("Cache-Control", "no-store");
                    return false;
                } catch (Exception e) {
                    response.setStatus(HttpServletResponse.SC_SERVICE_UNAVAILABLE);
                    response.setContentType("text/plain;charset=UTF-8");
                    response.getWriter().write("主节点网页地址配置无效，请检查 transfer.master-web-url");
                    return false;
                }
            }
            response.setContentType("application/json;charset=UTF-8");
            response.setStatus(HttpServletResponse.SC_FORBIDDEN);

            Map<String, Object> result = new HashMap<>();
            result.put("success", false);
            result.put("message", "当前节点不是主节点，请访问主节点: " + distributedConfig.getMasterIp());
            result.put("masterIp", distributedConfig.getMasterIp());
            result.put("masterPort", distributedConfig.getPort());

            response.getWriter().write(objectMapper.writeValueAsString(result));
            return false;
        }
        return true;
    }
}
