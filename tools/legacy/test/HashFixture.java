import java.lang.reflect.Method;

/** Calls the hash implementation in the pinned V4 JAR, without starting Spring. */
public final class HashFixture {
    public static void main(String[] args) throws Exception {
        Class<?> registry = Class.forName("com.transfer.tools.agent.AgentRegistryService");
        Method hash = registry.getDeclaredMethod("hash", String.class);
        hash.setAccessible(true);
        System.out.println(hash.invoke(null, args[0]));
    }
}
