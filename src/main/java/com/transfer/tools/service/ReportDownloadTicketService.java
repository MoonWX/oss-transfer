package com.transfer.tools.service;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;
import org.springframework.web.server.ResponseStatusException;

import java.security.SecureRandom;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.LongSupplier;

/** Short-lived, single-use authorization for browser-native streamed downloads. */
@Service
public class ReportDownloadTicketService {
    static final long LIFETIME_MS = 120_000;
    static final int MAX_TICKETS = 2048;
    private final Map<String, Ticket> tickets = new LinkedHashMap<>();
    private final SecureRandom random = new SecureRandom();
    private final LongSupplier clock;
    @Autowired private UserService userService;

    public ReportDownloadTicketService() { this(System::currentTimeMillis); }
    ReportDownloadTicketService(LongSupplier clock) { this.clock = clock; }

    public synchronized String issue(Integer taskId, String sessionToken) {
        String username = userService.getUsernameByToken(sessionToken);
        if (username == null) throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "登录已过期，请重新登录");
        long now = clock.getAsLong();
        tickets.entrySet().removeIf(entry -> entry.getValue().expiresAt <= now);
        // Bound memory even if an authenticated client never redeems its tickets.
        if (tickets.size() >= MAX_TICKETS) {
            throw new ResponseStatusException(HttpStatus.TOO_MANY_REQUESTS, "下载请求过多，请稍后重试");
        }
        byte[] bytes = new byte[32];
        random.nextBytes(bytes);
        String ticket = Base64.getUrlEncoder().withoutPadding().encodeToString(bytes);
        tickets.put(ticket, new Ticket(taskId, username, sessionToken, now + LIFETIME_MS));
        return ticket;
    }

    /** Also check the original login session: logging out revokes unredeemed tickets. */
    public synchronized String consume(String value, Integer taskId) {
        if (value == null || value.length() != 43) return null;
        Ticket ticket = tickets.remove(value);
        if (ticket == null || ticket.expiresAt <= clock.getAsLong() || !ticket.taskId.equals(taskId)) return null;
        return ticket.username.equals(userService.getUsernameByToken(ticket.sessionToken)) ? ticket.username : null;
    }

    private static class Ticket {
        final Integer taskId;
        final String username;
        final String sessionToken;
        final long expiresAt;
        Ticket(Integer taskId, String username, String sessionToken, long expiresAt) {
            this.taskId = taskId;
            this.username = username;
            this.sessionToken = sessionToken;
            this.expiresAt = expiresAt;
        }
    }
}
