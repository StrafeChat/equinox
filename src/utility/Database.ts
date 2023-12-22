import { cassandra } from ".."
import fs from "fs";

interface TypeDefinition {
    name: string;
    query: string;
}

export default class Database {

    /**
     * Load everything for the database.
     */
    public static init = async () => {
        await this.loadTypes();
        await this.loadTables();
        await this.loadIndexes();
        await this.loadMaterialViews();
    }

    /**
     * Load Types, not using fs to make sure that types are loaded in the correct order.
     */
    private static loadTypes = async () => {
        const types: TypeDefinition[] = [
            {
                name: "message_component",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_component (
                    type INT,
                    style INT,
                    label TEXT,
                    emoji TEXT,
                    custom_id TEXT,
                    url TEXT,
                    disabled BOOLEAN
                );`
            },
            {
                name: "message_embed_author",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_embed_author (
                    name TEXT,
                    url TEXT,
                    icon_url TEXT
                );`
            }, {
                name: "message_embed_field",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_embed_field (
                    name TEXT,
                    value TEXT,
                    inline BOOLEAN
                );`,
            }, {
                name: "message_embed_footer",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_embed_footer (
                    text TEXT,
                    icon_url TEXT
                );`,
            }, {
                name: "message_embed_field",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_embed_media (
                    url TEXT,
                    height INT,
                    width INT
                );`,
            }, {
                name: "message_embed",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_embed (
                    title TEXT,
                    description TEXT,
                    url TEXT,
                    timestamp TIMESTAMP,
                    color INT,
                    footer FROZEN<message_embed_footer>,
                    image FROZEN<message_embed_media>,
                    thumbnail FROZEN<message_embed_media>,
                    video FROZEN<message_embed_media>,
                    author FROZEN<message_embed_author>,
                    fields LIST<FROZEN<message_embed_field>>
                );`,
            }, {
                name: "reaction_count_details",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.reaction_count_details (
                    burst INT,
                    normal INT
                );`,
            }, {
                name: "message_reaction",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.message_reaction (
                    count INT,
                    count_details FROZEN<reaction_count_details>,
                    emoji TEXT,
                    burst_colors SET<INT>
                );`,
            }, {
                name: "user_presence",
                query: `
                CREATE TYPE IF NOT EXISTS ${cassandra.keyspace}.user_presence (
                    status TEXT,
                    status_text TEXT,
                    online BOOLEAN
                );`
            }
        ]

        for (const type of types) {
            try {
                await cassandra.execute(type.query);
            } catch (error) {
                console.error(`Error creating type: ${type.name}, ${error}`);
            }
        }
    }

    /**
     * Create all tables in the src/database/tables directory if they do not exist, the order does not matter.
     */
    private static loadTables = async () => {
        const tables = fs.readdirSync("src/database/tables");
        for (const table of tables) {
            try {
                const query = fs.readFileSync(`src/database/tables/${table}`).toString("utf-8").replace("{keyspace}", cassandra.keyspace);
                await cassandra.execute(query);
            } catch (error) {
                console.error(`Error creating table: ${table.split(".cql")[0]}, ${error}`);
            }
        }
    }

    /**
     * Create all indexes for tables in the src/database/indexes directory if they do not exist, the order does not matter.
     */
    private static loadIndexes = async () => {
        const indexes = fs.readdirSync("src/database/indexes");
        for (const index of indexes) {
            try {
                const query = fs.readFileSync(`src/database/indexes/${index}`).toString("utf-8").replace("{keyspace}", cassandra.keyspace);
                await cassandra.execute(query);
            } catch (error) {
                console.error(`Error creating index: ${index.split(".cql")[0]}, ${error}`);
            }
        }
    }


    /**
     * Create all materival views for tables in the src/database/material_views directory if they do not exist, the order does not matter.
     */
    private static loadMaterialViews = async () => {
        const materialViews = fs.readdirSync("src/database/material_views");
        for (const materialView of materialViews) {
            try {
                const queryPath = `src/database/material_views/${materialView}`;
                const query = fs.readFileSync(queryPath, 'utf-8');
                const modifiedQuery = query.split('{keyspace}').join(cassandra.keyspace);
                await cassandra.execute(modifiedQuery);
            } catch (error) {
                console.error(`Error creating material view: ${materialView.split(".cql")[0]}, ${error}`);
            }
        }
    }

}