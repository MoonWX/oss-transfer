package com.transfer.tools.util;

import com.transfer.tools.model.OssKeyMapping;
import com.transfer.tools.service.TransferValidationException;
import org.junit.Test;
import static org.junit.Assert.*;

public class OssKeyMapperTest {
    @Test public void preservesLiteralWhitespaceUnicodeAndSlashes() {
        String source = " /中文//x ";
        assertEquals(source, OssKeyMapper.destinationKey(null, source));
        assertEquals(source, OssKeyMapper.destinationKey(mapping(OssKeyMapping.Mode.PRESERVE), source));
    }
    @Test public void addsExactlyOneSeparatorWhenPrefixNeedsIt() {
        OssKeyMapping value = mapping(OssKeyMapping.Mode.ADD_PREFIX); value.setTargetPrefix(" copy ");
        assertEquals(" copy /a//b ", OssKeyMapper.destinationKey(value, "a//b "));
        value.setTargetPrefix("copy//"); assertEquals("copy///a", OssKeyMapper.destinationKey(value, "/a"));
    }
    @Test public void replacementIsLiteralAndCanRemovePrefix() {
        OssKeyMapping value = mapping(OssKeyMapping.Mode.REPLACE_PREFIX); value.setSourcePrefix("src/"); value.setTargetPrefix("dest");
        assertEquals("destchild", OssKeyMapper.destinationKey(value, "src/child"));
        value.setTargetPrefix(""); assertEquals("child", OssKeyMapper.destinationKey(value, "src/child"));
        invalid(() -> OssKeyMapper.destinationKey(value, "outside"));
        invalid(() -> OssKeyMapper.destinationKey(value, "src/"));
    }
    @Test public void exactTargetRequiresSingleSourceSelection() {
        OssKeyMapping value = mapping(OssKeyMapping.Mode.EXACT_KEY); value.setTargetKey("目标/ 文件 ");
        invalid(() -> OssKeyMapper.validate(value, null, null));
        invalid(() -> OssKeyMapper.validate(value, "src/", "src/key"));
        OssKeyMapper.validate(value, null, "src/key");
        assertEquals("目标/ 文件 ", OssKeyMapper.destinationKey(value, "src/key"));
    }
    @Test public void replacementRequiresMatchingFilterNotMerelyRangeStart() {
        OssKeyMapping value = mapping(OssKeyMapping.Mode.REPLACE_PREFIX); value.setSourcePrefix("src/");
        invalid(() -> OssKeyMapper.validate(value, null, null));
        invalid(() -> OssKeyMapper.validate(value, "other/", null));
        OssKeyMapper.validate(value, "src/", null);
    }
    @Test public void byteLimitIsUtf8AndIncludesMappedPrefix() {
        String key = repeat("中", 341) + "x";
        assertEquals(key, OssKeyMapper.destinationKey(null, key));
        invalid(() -> OssKeyMapper.destinationKey(null, key + "x"));
        OssKeyMapping value = mapping(OssKeyMapping.Mode.ADD_PREFIX); value.setTargetPrefix("x");
        invalid(() -> OssKeyMapper.destinationKey(value, key));
        invalid(() -> OssKeyMapper.destinationKey(null, "\ud800"));
    }
    @Test public void rejectsMissingModeAndFieldsFromOtherModes() {
        invalid(() -> OssKeyMapper.destinationKey(new OssKeyMapping(), "key"));
        OssKeyMapping value = mapping(OssKeyMapping.Mode.ADD_PREFIX);
        invalid(() -> OssKeyMapper.destinationKey(value, "key"));
        value.setTargetPrefix("ok"); value.setTargetKey("ignored-but-dangerous");
        invalid(() -> OssKeyMapper.destinationKey(value, "key"));
    }
    @Test public void nullAndExplicitPreserveAreEquivalent() {
        assertTrue(OssKeyMapper.equivalent(null, mapping(OssKeyMapping.Mode.PRESERVE)));
        assertFalse(OssKeyMapper.equivalent(null, mapping(OssKeyMapping.Mode.EXACT_KEY)));
    }
    private static OssKeyMapping mapping(OssKeyMapping.Mode mode) { OssKeyMapping value = new OssKeyMapping(); value.setMode(mode); return value; }
    private static String repeat(String value, int count) { StringBuilder result = new StringBuilder(); for (int i=0;i<count;i++) result.append(value); return result.toString(); }
    private static void invalid(Runnable action) { try { action.run(); fail("invalid mapping accepted"); } catch (TransferValidationException expected) { } }
}
