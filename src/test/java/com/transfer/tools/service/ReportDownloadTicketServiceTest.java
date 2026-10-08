package com.transfer.tools.service;

import org.junit.Before;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.web.server.ResponseStatusException;
import java.util.concurrent.atomic.AtomicLong;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class ReportDownloadTicketServiceTest {
    private final AtomicLong clock = new AtomicLong(1000);
    private final UserService users = mock(UserService.class);
    private final ReportDownloadTicketService tickets = new ReportDownloadTicketService(clock::get);

    @Before public void setup() {
        ReflectionTestUtils.setField(tickets, "userService", users);
        when(users.getUsernameByToken("session")).thenReturn("admin");
    }

    @Test public void ticketIsOpaqueTaskBoundAndSingleUse() {
        String ticket = tickets.issue(12, "session");
        assertEquals(43, ticket.length());
        assertFalse(ticket.contains("session"));
        assertEquals("admin", tickets.consume(ticket, 12));
        assertNull(tickets.consume(ticket, 12));
        assertNull(tickets.consume(tickets.issue(12, "session"), 13));
    }

    @Test public void expiredAndLoggedOutSessionsCannotDownload() {
        String expired = tickets.issue(12, "session");
        clock.addAndGet(ReportDownloadTicketService.LIFETIME_MS);
        assertNull(tickets.consume(expired, 12));
        String loggedOut = tickets.issue(12, "session");
        when(users.getUsernameByToken("session")).thenReturn(null);
        assertNull(tickets.consume(loggedOut, 12));
    }

    @Test public void outstandingTicketMemoryIsBoundedAndExpiryFreesCapacity() {
        for (int i = 0; i < ReportDownloadTicketService.MAX_TICKETS; i++) tickets.issue(12, "session");
        try {
            tickets.issue(12, "session");
            fail("capacity should reject outstanding tickets");
        } catch (ResponseStatusException expected) { assertEquals(429, expected.getStatus().value()); }
        clock.addAndGet(ReportDownloadTicketService.LIFETIME_MS);
        assertNotNull(tickets.issue(12, "session"));
    }

    @Test public void invalidSessionCannotIssueTicket() {
        try {
            tickets.issue(12, "invalid");
            fail("must authenticate first");
        } catch (ResponseStatusException expected) { assertEquals(401, expected.getStatus().value()); }
    }
}
