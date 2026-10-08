package com.transfer.tools.service;

import org.junit.Test;
import org.junit.Before;
import static org.junit.Assert.*;

public class BucketExploreServiceTest {

    private BucketExploreService service;

    @Before
    public void setUp() {
        service = new BucketExploreService();
    }

    // ==================== 基础ASCII字符测试 ====================

    @Test
    public void testCalculateNextMarker_BasicAscii() {
        assertEquals("aab", service.calculateNextMarker("aaa"));
        assertEquals("ab", service.calculateNextMarker("aa"));
        assertEquals("b", service.calculateNextMarker("a"));
        assertEquals("bcd", service.calculateNextMarker("bcc"));
    }

    @Test
    public void testCalculateNextMarker_SingleChar() {
        assertEquals("b", service.calculateNextMarker("a"));
        assertEquals("c", service.calculateNextMarker("b"));
        assertEquals("!", service.calculateNextMarker(" "));
        assertEquals("\"", service.calculateNextMarker("!"));
    }

    @Test
    public void testCalculateNextMarker_MaxPrintableChar() {
        assertEquals("", service.calculateNextMarker("~"));
        assertEquals("~~ ", service.calculateNextMarker("~~"));
    }

    @Test
    public void testCalculateNextMarker_EndsWithMaxChar() {
        assertEquals("bd", service.calculateNextMarker("bc~"));
        assertEquals("c", service.calculateNextMarker("b~"));
        assertEquals("ad", service.calculateNextMarker("ac~"));
    }

    @Test
    public void testCalculateNextMarker_AllTildes() {
        assertEquals("", service.calculateNextMarker("~"));
        assertEquals("~~ ", service.calculateNextMarker("~~"));
        assertEquals("~~~ ", service.calculateNextMarker("~~~"));
    }

    // ==================== 中文字符测试 ====================

    @Test
    public void testCalculateNextMarker_ChineseChars() {
        String result = service.calculateNextMarker("你");
        assertNotNull(result);
        assertFalse(result.isEmpty());
    }

    @Test
    public void testCalculateNextMarker_ChinesePrefix() {
        String result = service.calculateNextMarker("中文目录");
        assertNotNull(result);
        assertTrue(result.length() > 0);
    }

    @Test
    public void testCalculateNextMarker_MixedChineseAscii() {
        assertNotNull(service.calculateNextMarker("中文test"));
        assertNotNull(service.calculateNextMarker("test中文"));
        assertNotNull(service.calculateNextMarker("a中文b"));
    }

    @Test
    public void testCalculateNextMarker_ChineseEndsWithTilde() {
        String result = service.calculateNextMarker("中文~");
        assertNotNull(result);
        assertFalse(result.isEmpty());
    }

    // ==================== 特殊字符测试 ====================

    @Test
    public void testCalculateNextMarker_SpecialChars() {
        assertEquals("@", service.calculateNextMarker("?"));
        assertEquals("A", service.calculateNextMarker("@"));
        assertEquals("[", service.calculateNextMarker("Z"));
        assertEquals("a", service.calculateNextMarker("`"));
        assertEquals("{", service.calculateNextMarker("z"));
    }

    @Test
    public void testCalculateNextMarker_Symbols() {
        assertEquals(")", service.calculateNextMarker("("));
        assertEquals("+", service.calculateNextMarker("*"));
        assertEquals("-", service.calculateNextMarker(","));
        assertEquals("/", service.calculateNextMarker("."));
        assertEquals(":", service.calculateNextMarker("9"));
        assertEquals("<", service.calculateNextMarker(";"));
    }

    @Test
    public void testCalculateNextMarker_SpaceChar() {
        assertEquals("!", service.calculateNextMarker(" "));
        assertEquals("\"", service.calculateNextMarker("!"));
    }

    @Test
    public void testCalculateNextMarker_UnderscoreAndDash() {
        assertEquals("`", service.calculateNextMarker("_"));
        assertEquals(".", service.calculateNextMarker("-"));
    }

    @Test
    public void testCalculateNextMarker_DotAndSlash() {
        assertEquals("/", service.calculateNextMarker("."));
        assertEquals("0", service.calculateNextMarker("/"));
    }

    // ==================== 边界条件测试 ====================

    @Test
    public void testCalculateNextMarker_EmptyString() {
        assertEquals("", service.calculateNextMarker(""));
    }

    @Test
    public void testCalculateNextMarker_Null() {
        assertEquals("", service.calculateNextMarker(null));
    }

    @Test
    public void testCalculateNextMarker_SingleSpace() {
        assertEquals("!", service.calculateNextMarker(" "));
    }

    @Test
    public void testCalculateNextMarker_MinPrintableChar() {
        assertEquals("!", service.calculateNextMarker(" "));
        assertEquals(" !", service.calculateNextMarker("  "));
    }

    // ==================== 路径分隔符测试 ====================

    @Test
    public void testCalculateNextMarker_PathSeparators() {
        assertEquals("0", service.calculateNextMarker("/"));
        assertEquals(":", service.calculateNextMarker("9"));
        assertEquals("<", service.calculateNextMarker(";"));
    }

    @Test
    public void testCalculateNextMarker_CommonPathPatterns() {
        assertNotNull(service.calculateNextMarker("folder/"));
        assertNotNull(service.calculateNextMarker("folder/subfolder/"));
        assertNotNull(service.calculateNextMarker("a/b/c/"));
    }

    @Test
    public void testCalculateNextMarker_PathWithTilde() {
        String result = service.calculateNextMarker("folder/~");
        assertNotNull(result);
        assertFalse(result.isEmpty());
    }

    // ==================== 数字字符测试 ====================

    @Test
    public void testCalculateNextMarker_Numbers() {
        assertEquals("1", service.calculateNextMarker("0"));
        assertEquals("2", service.calculateNextMarker("1"));
        assertEquals(":", service.calculateNextMarker("9"));
        assertEquals("11", service.calculateNextMarker("10"));
        assertEquals("21", service.calculateNextMarker("20"));
    }

    @Test
    public void testCalculateNextMarker_NumberSequences() {
        assertEquals("1235", service.calculateNextMarker("1234"));
        assertEquals("1001", service.calculateNextMarker("1000"));
    }

    // ==================== 大小写字母测试 ====================

    @Test
    public void testCalculateNextMarker_UpperCase() {
        assertEquals("B", service.calculateNextMarker("A"));
        assertEquals("Z", service.calculateNextMarker("Y"));
        assertEquals("[", service.calculateNextMarker("Z"));
        assertEquals("BB", service.calculateNextMarker("BA"));
    }

    @Test
    public void testCalculateNextMarker_LowerCase() {
        assertEquals("b", service.calculateNextMarker("a"));
        assertEquals("z", service.calculateNextMarker("y"));
        assertEquals("{", service.calculateNextMarker("z"));
        assertEquals("bb", service.calculateNextMarker("ba"));
    }

    @Test
    public void testCalculateNextMarker_MixedCase() {
        assertNotNull(service.calculateNextMarker("AbCd"));
        assertNotNull(service.calculateNextMarker("aBcD"));
        assertNotNull(service.calculateNextMarker("ABC"));
        assertNotNull(service.calculateNextMarker("abc"));
    }

    // ==================== 复杂场景测试 ====================

    @Test
    public void testCalculateNextMarker_LongString() {
        String longPrefix = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
        String result = service.calculateNextMarker(longPrefix);
        assertNotNull(result);
        assertTrue(result.length() > 0);
    }

    @Test
    public void testCalculateNextMarker_MixedChars() {
        assertNotNull(service.calculateNextMarker("a1b2c3"));
        assertNotNull(service.calculateNextMarker("test-123_abc"));
        assertNotNull(service.calculateNextMarker("folder/file.txt"));
    }

    @Test
    public void testCalculateNextMarker_UnicodeChars() {
        assertNotNull(service.calculateNextMarker("🎉"));
        assertNotNull(service.calculateNextMarker("日本語"));
        assertNotNull(service.calculateNextMarker("한국어"));
        assertNotNull(service.calculateNextMarker("العربية"));
    }

    @Test
    public void testCalculateNextMarker_EmojiAndSpecialUnicode() {
        assertNotNull(service.calculateNextMarker("😀"));
        assertNotNull(service.calculateNextMarker("📁folder"));
        assertNotNull(service.calculateNextMarker("test🎉"));
    }

    // ==================== 实际业务场景测试 ====================

    @Test
    public void testCalculateNextMarker_BucketNameLike() {
        assertNotNull(service.calculateNextMarker("my-bucket"));
        assertNotNull(service.calculateNextMarker("bucket-2024-06"));
        assertNotNull(service.calculateNextMarker("backup_20240615"));
    }

    @Test
    public void testCalculateNextMarker_S3KeyLike() {
        assertNotNull(service.calculateNextMarker("photos/2024/06/"));
        assertNotNull(service.calculateNextMarker("data/2024-06-15/"));
        assertNotNull(service.calculateNextMarker("logs/app-2024-06-15.log"));
    }

    @Test
    public void testCalculateNextMarker_PrefixPatterns() {
        assertNotNull(service.calculateNextMarker("prefix_"));
        assertNotNull(service.calculateNextMarker("prefix-"));
        assertNotNull(service.calculateNextMarker("prefix."));
    }

    @Test
    public void testCalculateNextMarker_VersionedKeys() {
        assertNotNull(service.calculateNextMarker("file_v1"));
        assertNotNull(service.calculateNextMarker("file_v2"));
        assertNotNull(service.calculateNextMarker("2024-06-15T10:30:00Z"));
    }

    // ==================== 性能相关测试 ====================

    @Test
    public void testCalculateNextMarker_DeepNestedPath() {
        String deepPath = "a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/";
        String result = service.calculateNextMarker(deepPath);
        assertNotNull(result);
    }

    @Test
    public void testCalculateNextMarker_ManyTildesInMiddle() {
        String prefix = "test~~~data";
        String result = service.calculateNextMarker(prefix);
        assertNotNull(result);
    }

    // ==================== 连续调用测试 ====================

    @Test
    public void testCalculateNextMarker_SequentialCalls() {
        String marker = "aaa";
        marker = service.calculateNextMarker(marker);
        assertEquals("aab", marker);
        marker = service.calculateNextMarker(marker);
        assertEquals("aac", marker);
        marker = service.calculateNextMarker(marker);
        assertEquals("aad", marker);
    }

    @Test
    public void testCalculateNextMarker_SequentialFromStart() {
        String marker = "a";
        marker = service.calculateNextMarker(marker);
        assertEquals("b", marker);
        marker = service.calculateNextMarker(marker);
        assertEquals("c", marker);
    }

    // ==================== 错误恢复测试 ====================

    @Test
    public void testCalculateNextMarker_AfterEmptyResult() {
        String result = service.calculateNextMarker("~");
        assertEquals("", result);

        result = service.calculateNextMarker("a");
        assertEquals("b", result);
    }

    @Test
    public void testCalculateNextMarker_InvalidCharsRecovery() {
        assertNotNull(service.calculateNextMarker("\0\0\0"));
        assertNotNull(service.calculateNextMarker("\n\t\r"));
    }

    // ==================== findCommonPrefix 相关测试 ====================

    @Test
    public void testFindCommonPrefix_SamePrefix() {
        String result = invokeFindCommonPrefix("abc123", "abc456");
        assertEquals("abc", result);
    }

    @Test
    public void testFindCommonPrefix_NoCommonPrefix() {
        String result = invokeFindCommonPrefix("abc", "xyz");
        assertNull(result);
    }

    @Test
    public void testFindCommonPrefix_IdenticalStrings() {
        String result = invokeFindCommonPrefix("test", "test");
        assertEquals("test", result);
    }

    @Test
    public void testFindCommonPrefix_EmptyStrings() {
        assertNull(invokeFindCommonPrefix("", ""));
        assertNull(invokeFindCommonPrefix("", "test"));
        assertNull(invokeFindCommonPrefix("test", ""));
    }

    @Test
    public void testFindCommonPrefix_NullInputs() {
        assertNull(invokeFindCommonPrefix(null, "test"));
        assertNull(invokeFindCommonPrefix("test", null));
        assertNull(invokeFindCommonPrefix(null, null));
    }

    @Test
    public void testFindCommonPrefix_ChineseChars() {
        String result = invokeFindCommonPrefix("中文目录1", "中文目录2");
        assertEquals("中文目录", result);
    }

    @Test
    public void testFindCommonPrefix_MixedChars() {
        String result = invokeFindCommonPrefix("folder/文件1", "folder/文件2");
        assertEquals("folder/文件", result);
    }

    @Test
    public void testFindCommonPrefix_SpecialChars() {
        String result = invokeFindCommonPrefix("test-123_a", "test-123_b");
        assertEquals("test-123_", result);
    }

    private String invokeFindCommonPrefix(String key1, String key2) {
        try {
            java.lang.reflect.Method method = BucketExploreService.class.getDeclaredMethod(
                "findCommonPrefix", String.class, String.class);
            method.setAccessible(true);
            return (String) method.invoke(service, key1, key2);
        } catch (Exception e) {
            throw new RuntimeException(e);
        }
    }
}
